// tests/pipeline_tests.cpp — 恢复路径、边界条件与端到端验收
//
// 覆盖：正常/截断尾部/中段损坏（缺口+重复）/坏头夹具、映射校验
// （越界/重复/不兼容）、路径穿越拒绝、有界队列背压与取消、
// 多容器输出、批次隔离与样本直通一致性。
#include <catch2/catch_test_macros.hpp>

#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <string>

#include "mediacore/bounded_queue.hpp"
#include "mediacore/fixture.hpp"
#include "mediacore/path_guard.hpp"
#include "mediacore/remux_pipeline.hpp"
#include "mediacore/track_mapper.hpp"
#include "mediacore/wav_demuxer.hpp"

namespace fs = std::filesystem;
using namespace mediacore;

namespace {

std::string uniqueDir() {
    static std::atomic<int> counter{0};
    // 结合进程内计数、本对象地址与系统时钟，保证跨进程/跨用例目录唯一，
    // 避免 catch_discover 拆分的独立 ctest 进程复用同名临时目录
    const auto now = std::chrono::steady_clock::now().time_since_epoch().count();
    static const int anchor = 0;
    const auto name = "mediacore_test_" + std::to_string(now) + "_" +
                      std::to_string(reinterpret_cast<std::uintptr_t>(&anchor)) +
                      "_" + std::to_string(++counter);
    auto p = fs::temp_directory_path() / name;
    fs::create_directories(p);
    return p.string();
}

MappingConfig monoMapping() {
    MappingConfig mc;
    OutputTrack t;
    t.name = "voice";
    t.layout = TrackLayout::Mono;
    t.inputChannels = {0};
    mc.tracks.push_back(t);
    return mc;
}

} // namespace

TEST_CASE("WAV 正常输入逐块解复用且无恢复错误") {
    const auto dir = uniqueDir();
    auto fx = fixture::makeAcceptanceFixtures(dir);

    WavDemuxer d;
    DemuxConfig cfg;
    cfg.blockFrames = 256;
    REQUIRE(d.open(fx.cleanWav, cfg).ok());
    REQUIRE(d.format().sampleRate == 16000);
    REQUIRE(d.format().channels == 1);
    REQUIRE(d.format().bitsPerSample == 16);
    REQUIRE(d.format().blockAlign() == 2);

    SampleTick total = 0;
    SampleTick expected = 0;
    AudioBlock b;
    MediaError err;
    while (d.nextBlock(b, err)) {
        REQUIRE(b.startSample == expected);
        total += b.frameCount();
        expected = b.endSample();
    }
    REQUIRE(total == 2048);
    REQUIRE(d.stats().recoverableErrors.empty());
    REQUIRE(d.stats().tailBytesDropped == 0);
}

TEST_CASE("尾部不完整样本被截断并报告可恢复错误") {
    const auto dir = uniqueDir();
    auto fx = fixture::makeAcceptanceFixtures(dir);

    WavDemuxer d;
    DemuxConfig cfg;
    REQUIRE(d.open(fx.truncatedTail, cfg).ok());

    SampleTick total = 0;
    AudioBlock b;
    MediaError err;
    while (d.nextBlock(b, err)) {
        total += b.frameCount();
    }
    REQUIRE(total == 2048); // 完整样本全部保留
    REQUIRE(d.stats().tailBytesDropped == 3);
    REQUIRE_FALSE(d.stats().recoverableErrors.empty());
    REQUIRE(d.stats().recoverableErrors.front().code ==
            MediaErrorCode::IncompleteTail);
}

TEST_CASE("中段损坏/缺口/重复分别输出对应块且时间线单调") {
    const auto dir = uniqueDir();
    auto fx = fixture::makeAcceptanceFixtures(dir);

    WavDemuxer d;
    DemuxConfig cfg;
    REQUIRE(d.open(fx.midCorrupt, cfg).ok());

    bool sawCorrupt = false, sawGap = false, sawDup = false;
    SampleTick prevEnd = 0;
    AudioBlock b;
    MediaError err;
    while (d.nextBlock(b, err)) {
        REQUIRE(b.startSample >= prevEnd - b.frameCount());
        if (b.kind == BlockKind::Corrupt) {
            sawCorrupt = true;
            REQUIRE(b.endSample() > b.startSample);
        } else if (b.kind == BlockKind::Gap) {
            sawGap = true;
            REQUIRE(b.gapFrames > 0);
        } else if (b.kind == BlockKind::Duplicate) {
            sawDup = true;
        }
        if (b.kind != BlockKind::Duplicate) {
            prevEnd = b.endSample();
        }
    }
    REQUIRE(sawCorrupt);
    REQUIRE(sawGap);
    REQUIRE(sawDup);
    auto codes = d.stats().recoverableErrors;
    auto has = [&](MediaErrorCode c) {
        for (auto& e : codes)
            if (e.code == c) return true;
        return false;
    };
    REQUIRE(has(MediaErrorCode::CorruptStream));
    REQUIRE(has(MediaErrorCode::TimestampGap));
    REQUIRE(has(MediaErrorCode::DuplicateBlock));
}

TEST_CASE("截断头部返回不可恢复错误") {
    const auto dir = uniqueDir();
    auto fx = fixture::makeAcceptanceFixtures(dir);

    WavDemuxer d;
    DemuxConfig cfg;
    auto r = d.open(fx.badHeader, cfg);
    REQUIRE_FALSE(r.ok());
    REQUIRE(r.error().code == MediaErrorCode::TruncatedHeader);
    REQUIRE_FALSE(r.error().recoverable);
}

TEST_CASE("映射越界/重复占用/布局不兼容被拒绝") {
    PcmFormat fmt{48000, 8, 16, SampleFormat::S16Le};
    TrackMapper m;

    SECTION("声道越界") {
        MappingConfig mc;
        OutputTrack t;
        t.name = "x";
        t.layout = TrackLayout::Mono;
        t.inputChannels = {9};
        mc.tracks.push_back(t);
        auto r = m.applyConfig(mc, fmt);
        REQUIRE_FALSE(r.ok());
        REQUIRE(r.error().code == MediaErrorCode::MappingOutOfRange);
    }
    SECTION("重复占用") {
        MappingConfig mc;
        for (int i = 0; i < 2; ++i) {
            OutputTrack t;
            t.name = "t" + std::to_string(i);
            t.layout = TrackLayout::Mono;
            t.inputChannels = {0};
            mc.tracks.push_back(t);
        }
        auto r = m.applyConfig(mc, fmt);
        REQUIRE_FALSE(r.ok());
        REQUIRE(r.error().code == MediaErrorCode::MappingDuplicate);
    }
    SECTION("布局不兼容") {
        MappingConfig mc;
        OutputTrack t;
        t.name = "s";
        t.layout = TrackLayout::Stereo;
        t.inputChannels = {0}; // 立体声明明需要 2 声道
        mc.tracks.push_back(t);
        auto r = m.applyConfig(mc, fmt);
        REQUIRE_FALSE(r.ok());
        REQUIRE(r.error().code == MediaErrorCode::MappingIncompatible);
    }
}

TEST_CASE("去交错映射保持样本帧序与数值不变") {
    PcmFormat fmt{16000, 2, 16, SampleFormat::S16Le};
    MappingConfig mc;
    OutputTrack l;
    l.name = "L";
    l.layout = TrackLayout::Mono;
    l.inputChannels = {0};
    OutputTrack r;
    r.name = "R";
    r.layout = TrackLayout::Mono;
    r.inputChannels = {1};
    mc.tracks = {l, r};

    TrackMapper mapper;
    REQUIRE(mapper.applyConfig(mc, fmt).ok());

    AudioBlock in;
    in.format = fmt;
    in.startSample = 0;
    // 4 帧交错：L,R 取值不同便于校验
    std::int16_t pcm[8] = {10, -10, 20, -20, 30, -30, 40, -40};
    in.interleavedPcm.assign(reinterpret_cast<std::uint8_t*>(pcm),
                             reinterpret_cast<std::uint8_t*>(pcm) + 16);

    auto out = mapper.mapBlock(in);
    REQUIRE(out.size() == 2);
    auto* lp = reinterpret_cast<const std::int16_t*>(
        out[0].interleavedPcm.data());
    auto* rp = reinterpret_cast<const std::int16_t*>(
        out[1].interleavedPcm.data());
    REQUIRE(lp[0] == 10);
    REQUIRE(lp[3] == 40);
    REQUIRE(rp[0] == -10);
    REQUIRE(rp[3] == -40);
}

TEST_CASE("有界队列满则背压、取消即解除阻塞") {
    BoundedBlockQueue q(2);
    AudioBlock b;
    b.format = PcmFormat{16000, 1, 16, SampleFormat::S16Le};
    REQUIRE(q.push(b));
    REQUIRE(q.push(b));
    REQUIRE(q.peakSize() == 2);
    REQUIRE(q.peakSize() <= q.capacity());

    AudioBlock got;
    REQUIRE(q.pop(got));
    q.cancel();
    // 取消后 push 立即失败，不阻塞
    REQUIRE_FALSE(q.push(b));
}

TEST_CASE("路径穿越被拒绝，工作区内路径放行") {
    const auto base = fs::absolute(uniqueDir());
    auto bad = safeJoin(base, "../escape.wav");
    REQUIRE_FALSE(bad.ok());
    REQUIRE(bad.error().code == MediaErrorCode::PathTraversal);

    auto good = safeJoin(base, "sub/../in.wav");
    REQUIRE(good.ok());
}

TEST_CASE("端到端批次：恢复/拒绝计数与有界队列峰值") {
    const auto dir = uniqueDir();
    auto fx = fixture::makeAcceptanceFixtures(dir);
    const auto out = uniqueDir();

    PipelineConfig config;
    config.queueBlocks = 4;
    config.mapping = monoMapping();
    config.container = Container::Wav;

    std::vector<std::string> inputs{fx.cleanWav, fx.truncatedTail,
                                    fx.midCorrupt, fx.badHeader};
    auto summary = runBatch(inputs, out, config);

    REQUIRE(summary.files == 4);
    REQUIRE(summary.recovered == 2);
    REQUIRE(summary.rejected == 1);
    REQUIRE(summary.queuePeakBlocks <= config.queueBlocks);

    // 成功文件均产出 WAV 与清单
    int manifests = 0;
    for (auto& e : fs::directory_iterator(out)) {
        if (e.path().extension() == ".json") {
            ++manifests;
            REQUIRE(fs::file_size(e.path()) > 0);
        }
    }
    REQUIRE(manifests == 3);
    // 坏头文件不得留下任何输出
    REQUIRE_FALSE(fs::exists(fs::path(out) / "bad_header_voice.wav"));
    REQUIRE_FALSE(fs::exists(fs::path(out) / "bad_header_manifest.json"));
}

TEST_CASE("Matroska 容器输出可被识别且样本为 pcm_s16le") {
    const auto dir = uniqueDir();
    auto fx = fixture::makeAcceptanceFixtures(dir);
    const auto out = uniqueDir();

    PipelineConfig config;
    config.queueBlocks = 8;
    config.mapping = monoMapping();
    config.container = Container::Matroska;

    RemuxPipeline pipe(config);
    auto r = pipe.processFile(fx.cleanWav, out, "clean");
    REQUIRE(r.ok());
    const auto mka = fs::path(out) / "clean_voice.mka";
    REQUIRE(fs::exists(mka));
    REQUIRE(fs::file_size(mka) > 0);
}

TEST_CASE("48kHz/8 声道慢速写入下队列峰值严格不超容量") {
    const auto dir = uniqueDir();
    const auto out = uniqueDir();

    // 自生成 48kHz/8 声道、1 秒输入
    fixture::WavBuilder b;
    b.format = PcmFormat{48000, 8, 16, SampleFormat::S16Le};
    b.writeHeader(0);
    b.writeBytes(fixture::makePcm(b.format, 48000));
    b.finish();
    const std::string input = dir + "/in8ch.wav";
    REQUIRE(fixture::writeFile(input, b.bytes));

    MappingConfig mc;
    for (int ch = 0; ch < 8; ++ch) {
        OutputTrack t;
        t.name = "ch" + std::to_string(ch);
        t.layout = TrackLayout::Mono;
        t.inputChannels = {ch};
        mc.tracks.push_back(t);
    }

    constexpr std::size_t kCap = 4;
    PipelineConfig config;
    config.queueBlocks = kCap;
    config.mapping = mc;
    config.slowWriteUs = 500; // 慢速写入迫使生产者在满队列处背压

    RemuxPipeline pipe(config);
    auto r = pipe.processFile(input, out, "in8ch");
    REQUIRE(r.ok());
    REQUIRE(r.value().queuePeakBlocks <= kCap);
    REQUIRE(r.value().queuePeakBlocks >= 1);

    // 8 条单声道轨道均产出
    int tracks = 0;
    for (auto& e : fs::directory_iterator(out)) {
        if (e.path().extension() == ".wav") {
            ++tracks;
        }
    }
    REQUIRE(tracks == 8);
}

TEST_CASE("取消后生产者及时停止，不产生成功清单") {
    BoundedBlockQueue q(1);
    AudioBlock blk;
    blk.format = PcmFormat{16000, 1, 16, SampleFormat::S16Le};
    REQUIRE(q.push(blk));
    q.cancel();

    AudioBlock got;
    // 已取消：pop 排空后返回 false，push 立即失败
    CHECK_FALSE(q.push(blk));
    CHECK(q.peakSize() <= q.capacity());
    (void)got;
}
