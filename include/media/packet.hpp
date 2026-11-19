#pragma once
// 包与边界状态模型。核心模块不依赖 FFmpeg：解封装器把 AVPacket 转成
// MediaPacket，下游全部在统一时间基上工作。
#include <cstdint>
#include <vector>
#include "media/time_model.hpp"

namespace media {

enum class MediaType { Video, Audio };
const char* toString(MediaType t);

// 流式数据包（持有自有缓冲，边界清晰，便于有界队列搬运）。
struct MediaPacket {
  MediaType type = MediaType::Video;
  size_t index = 0;          // 轨道内单调序号
  size_t globalIndex = 0;    // 全轨累计序号（诊断定位用）
  int64_t rawPts = 0;        // 容器原始 PTS（轨道时间基滴答）
  int64_t rawDts = 0;
  int64_t durationTicks = 0; // 原始时长（滴答）
  int64_t repairedUs = 0;    // 修复后统一时间基 PTS（微秒）
  int64_t durationUs = 0;    // 修复后时长（微秒）
  bool keyframe = false;
  bool corrupt = false;      // 已知损坏（侧车清单标注/解码失败回填）
  bool duplicate = false;    // 重复 PTS 标记
  bool deliverable = true;   // 致命歧义区段置 false
  std::vector<uint8_t> payload;
};

// 转场/剪辑边界落在时间线上的状态。
enum class BoundaryKind { Exact, Snapped, MissingFrame };
const char* toString(BoundaryKind k);

struct BoundaryState {
  size_t editIndex = 0;
  int64_t targetUs = 0;       // 清单要求的边界时间
  int64_t actualUs = 0;       // 最近有效帧时间
  int64_t errorUs = 0;        // actual - target
  BoundaryKind kind = BoundaryKind::Exact;
  size_t nearestPacket = 0;   // 最近有效帧序号
};

// 交叉淡化转场：重叠区内同时保留两路来源。
struct Transition {
  size_t editIndex = 0;
  std::string fromTrack;
  std::string toTrack;
  int64_t startUs = 0;
  int64_t endUs = 0;
  bool contains(int64_t us) const { return us >= startUs && us < endUs; }
};

} // namespace media
