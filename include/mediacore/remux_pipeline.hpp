// mediacore/remux_pipeline.hpp — 分阶段流水线编排
//
// 解复用线程 -> BoundedBlockQueue（背压）-> 消费线程（映射 + 封装 +
// 质量统计 + 清单）。单文件处理结果以 FileResult 返回；多文件批次由
// BatchRunner 驱动，单文件失败保留已完成输出并继续后续文件。
#pragma once

#include <atomic>
#include <memory>
#include <string>
#include <thread>
#include <vector>

#include "mediacore/bounded_queue.hpp"
#include "mediacore/manifest.hpp"
#include "mediacore/remux_muxer.hpp"
#include "mediacore/track_mapper.hpp"
#include "mediacore/wav_demuxer.hpp"

namespace mediacore {

struct PipelineConfig {
    DemuxConfig demux{};
    MappingConfig mapping{};
    Container container = Container::Wav;
    std::size_t queueBlocks = 8;   // 固定队列容量（块数）
    int targetKbps = 0;            // 目标码率（仅统计对照，不重编码）
    std::int64_t slowWriteUs = 0;  // 模拟慢速写入（背压验证用）
};

struct FileResult {
    std::string input;
    bool ok = false;
    bool recovered = false;
    bool rejected = false;         // 不可恢复拒绝
    MediaError fatalError{};
    Manifest manifest{};
    std::vector<std::string> outputFiles;
    std::size_t queuePeakBlocks = 0;
};

class RemuxPipeline {
public:
    explicit RemuxPipeline(PipelineConfig config);
    ~RemuxPipeline();

    // 处理单个文件；outDir 必须已通过路径守卫校验
    Result<FileResult> processFile(const std::string& input,
                                   const std::string& outDir,
                                   const std::string& stem);

    void cancel() { cancelled_.store(true); }
    bool cancelled() const { return cancelled_.load(); }

private:
    PipelineConfig config_;
    std::atomic<bool> cancelled_{false};
};

struct BatchSummary {
    int files = 0;
    int recovered = 0;
    int rejected = 0;
    std::size_t queuePeakBlocks = 0;
    std::vector<FileResult> results;
};

// 批次处理：单个输入失败不影响已完成输出与后续文件
BatchSummary runBatch(const std::vector<std::string>& inputs,
                      const std::string& outDir,
                      const PipelineConfig& config);

} // namespace mediacore
