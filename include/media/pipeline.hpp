#pragma once
// 交付流水线编排：解封装 -> 时钟修复 -> 漂移估计 -> 有界队列背压处理
// -> 关键帧索引 -> 清单对齐 -> 质量统计，汇总为统一报告。
#include <map>
#include <string>
#include <vector>
#include "media/clock_repair.hpp"
#include "media/decoder.hpp"
#include "media/demuxer.hpp"
#include "media/index_builder.hpp"
#include "media/manifest.hpp"
#include "media/quality_report.hpp"

namespace media {

struct PipelineConfig {
  std::string workDir = "demo_out";
  std::string manifestPath = "demo_out/manifest.txt";
  std::string jsonPath;
  std::string indexPath;
  std::string diagPath;
  int queuePackets = 64;
  int truncateAt = 0;
  int64_t targetBitrateBps = 1'800'000;
};

struct TrackResult {
  std::string id;
  MediaType type = MediaType::Video;
  std::vector<MediaPacket> packets;
  RepairSummary repair;
  KeyframeIndex index;
  std::vector<BoundaryState> boundaries;
  double seekErrorMs = 0;
  DecodeOutcome decode;
  bool present = false;
};

struct PipelineReport {
  QualityStats stats;
  TimeError fatal = TimeError::None;
  size_t firstBadPacket = 0;
  std::string fatalDetail;
  std::map<std::string, TrackResult> tracks;
};

// 运行完整流水线。返回码语义由 main 映射：None=0，致命错误=2。
TimeError runPipeline(const PipelineConfig& cfg, PipelineReport& rep);

// 把各轨结果聚合为验收字段（码率/PSNR/对齐/落点/漂移）。
QualityStats aggregateStats(const PipelineReport& rep, size_t queuePeak,
                            int64_t targetBitrateBps, int64_t durationUs);

// 统计工作目录下该清单引用的全部媒体文件总字节（用于真实码率）。
uint64_t mediaBytesOnDisk(const std::string& dir, const Manifest& m);

// 读取清单文本并按轨道聚合侧车（多轨侧车文件名 <id>.sidecar）。
bool loadManifestAndSidecars(const std::string& dir, const std::string& manifest,
                             Manifest& m, std::map<std::string, Sidecar>& cars,
                             std::string& err);
} // namespace media
