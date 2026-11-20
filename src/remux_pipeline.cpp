#include "mediacore/remux_pipeline.hpp"

#include <algorithm>
#include <chrono>
#include <filesystem>
#include <thread>

#include "mediacore/quality.hpp"

namespace mediacore {

namespace fs = std::filesystem;

RemuxPipeline::RemuxPipeline(PipelineConfig config)
    : config_(std::move(config)) {}

RemuxPipeline::~RemuxPipeline() = default;

Result<FileResult> RemuxPipeline::processFile(const std::string& input,
                                              const std::string& outDir,
                                              const std::string& stem) {
    FileResult result;
    result.input = input;

    WavDemuxer demuxer;
    auto opened = demuxer.open(input, config_.demux);
    if (!opened) {
        result.rejected = true;
        result.fatalError = opened.error();
        return result; // 不可恢复（截断头部/扫描超限/格式不支持）
    }
    const PcmFormat fmt = demuxer.format();

    TrackMapper mapper;
    auto mapped = mapper.applyConfig(config_.mapping, fmt);
    if (!mapped) {
        result.rejected = true;
        result.fatalError = mapped.error();
        return result;
    }

    // 输出文件与清单路径
    const std::string ext = containerExtension(config_.container);
    std::vector<std::string> outPaths;
    for (const auto& tr : config_.mapping.tracks) {
        outPaths.push_back(outDir + "/" + stem + "_" + tr.name + ext);
    }
    const std::string manifestPath = outDir + "/" + stem + "_manifest.json";

    // 每轨道一个独立输出文件（WAV 仅支持单音频流；MKA 同样按轨拆分，
    // 便于传输侧独立映射与码率核算）
    std::vector<std::unique_ptr<RemuxMuxer>> muxers;
    for (std::size_t i = 0; i < config_.mapping.tracks.size(); ++i) {
        auto m = std::make_unique<RemuxMuxer>();
        MappingConfig single;
        single.tracks.push_back(config_.mapping.tracks[i]);
        auto mo = m->open(outPaths[i], config_.container, single.tracks,
                          fmt.sampleRate);
        if (!mo) {
            result.rejected = true;
            result.fatalError = mo.error();
            return result;
        }
        muxers.push_back(std::move(m));
    }

    BoundedBlockQueue queue(config_.queueBlocks);
    QualityAnalyzer quality(fmt.sampleRate);
    std::vector<SkippedSpan> skipped;
    SampleTick expected = 0;          // 连续样本期望刻度
    SampleTick validStart = -1;
    SampleTick validEnd = 0;
    std::uint64_t rawBytes = 0;

    // 生产者：增量解复用，队列满即阻塞，取消即停
    std::thread producer([&] {
        AudioBlock blk;
        MediaError err;
        while (!cancelled_) {
            if (!demuxer.nextBlock(blk, err)) {
                break; // EOF
            }
            if (!queue.push(std::move(blk))) {
                cancelled_.store(true); // 取消：停止读取
                break;
            }
        }
    });

    // 消费者：映射 -> 封装 -> 统计
    bool writeOk = true;
    MediaError writeErr{};
    AudioBlock blk;
    while (queue.pop(blk)) {
        if (config_.slowWriteUs > 0) {
            std::this_thread::sleep_for(
                std::chrono::microseconds(config_.slowWriteUs));
        }
        rawBytes += blk.interleavedPcm.size();

        if (blk.kind == BlockKind::Corrupt ||
            blk.kind == BlockKind::Gap ||
            blk.kind == BlockKind::Duplicate) {
            const SampleTick end = blk.startSample +
                (blk.kind == BlockKind::Duplicate ? blk.frameCount() : 0);
            skipped.push_back(
                {{blk.startSample, end},
                 errorCodeName(blk.kind == BlockKind::Corrupt
                                   ? MediaErrorCode::CorruptStream
                               : blk.kind == BlockKind::Gap
                                   ? MediaErrorCode::TimestampGap
                                   : MediaErrorCode::DuplicateBlock),
                 "skipped by recovery"});
            quality.addSkippedRange({blk.startSample, end});
            expected = end; // 后续有效块从该刻度接续
            continue;
        }

        if (validStart < 0) {
            validStart = blk.startSample;
        }
        quality.addValidBlock(blk, expected);
        auto mappedBlocks = mapper.mapBlock(blk);
        for (std::size_t ti = 0; ti < mappedBlocks.size(); ++ti) {
            auto wrote = muxers[ti]->writeBlock(mappedBlocks[ti]);
            if (!wrote) {
                writeErr = wrote.error();
                writeOk = false;
                break;
            }
        }
        if (!writeOk) {
            queue.cancel();
            cancelled_.store(true);
            break;
        }
        expected = blk.endSample();
        validEnd = blk.endSample();
    }
    producer.join();
    result.queuePeakBlocks = queue.peakSize();

    if (!writeOk) {
        for (auto& m : muxers) {
            m->abort(); // 清理全部 .part，不产出伪成功清单
        }
        std::error_code ec;
        fs::remove(manifestPath + ".part", ec);
        result.rejected = true;
        result.fatalError = writeErr;
        return result;
    }

    std::uint64_t actualBytes = 0;
    for (auto& m : muxers) {
        auto committed = m->commit();
        if (!committed) {
            result.rejected = true;
            result.fatalError = committed.error();
            return result;
        }
        actualBytes += m->bytesWritten();
    }

    if (validStart < 0) {
        validStart = 0;
    }
    SampleRange validRange{validStart, validEnd};
    auto stats = demuxer.stats();
    auto q = quality.finalize(validRange, rawBytes,
                              actualBytes, config_.targetKbps);

    Manifest manifest;
    manifest.sourceFile = fs::path(input).filename().string();
    manifest.sampleRate = fmt.sampleRate;
    manifest.validRange = validRange;
    manifest.skipped = skipped;
    manifest.quality = q;
    manifest.recovered = !stats.recoverableErrors.empty();
    for (const auto& e : stats.recoverableErrors) {
        manifest.recoverableCodes.push_back(errorCodeName(e.code));
    }
    for (std::size_t i = 0; i < config_.mapping.tracks.size(); ++i) {
        TrackManifest tm;
        tm.name = config_.mapping.tracks[i].name;
        tm.layout = trackLayoutName(config_.mapping.tracks[i].layout);
        tm.outputFile = fs::path(outPaths[i]).filename().string();
        tm.validRange = validRange;
        tm.alignmentErrorSamples =
            validRange.start == 0 ? 0 : validRange.start;
        manifest.tracks.push_back(tm);
    }
    auto mw = writeManifest(manifestPath, manifest);
    if (!mw) {
        result.rejected = true;
        result.fatalError = mw.error();
        return result;
    }

    result.ok = true;
    result.recovered = manifest.recovered;
    result.manifest = std::move(manifest);
    result.outputFiles = outPaths;
    result.outputFiles.push_back(manifestPath);
    return result;
}

BatchSummary runBatch(const std::vector<std::string>& inputs,
                      const std::string& outDir,
                      const PipelineConfig& config) {
    BatchSummary summary;
    summary.files = static_cast<int>(inputs.size());
    for (const auto& input : inputs) {
        RemuxPipeline pipeline(config);
        const std::string stem =
            fs::path(input).stem().string();
        auto r = pipeline.processFile(input, outDir, stem);
        if (!r) {
            ++summary.rejected;
            summary.results.push_back(r.valueOr(FileResult{}));
            continue; // 单文件失败：保留已完成输出，继续后续文件
        }
        FileResult fr = std::move(r.value());
        if (fr.recovered) {
            ++summary.recovered;
        }
        summary.queuePeakBlocks =
            std::max(summary.queuePeakBlocks, fr.queuePeakBlocks);
        summary.results.push_back(std::move(fr));
    }
    return summary;
}

} // namespace mediacore
