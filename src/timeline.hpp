// 多轨时间线合成：把节目、字幕、媒体轨的片段按确定性规则裁定为不重叠区段。
// 裁定规则（全部记录原因与源位置）：
//  - 负起点 / 零长度 / 越过轨边界：拒绝该片段；
//  - 空轨：记录但不产出区段；
//  - 重叠：高优先级轨胜出并占用，低优先级片段在重叠区被裁掉；
//    同优先级按源顺序（先到先得），后者被裁。任何阶段不做随机选择。
#pragma once
#include <cstdint>
#include <string>
#include <vector>

#include "util.hpp"

namespace mb {

struct Clip {
  std::string source_ref;            // 来源文件/轨标识
  int64_t start = 0;                 // 主刻度
  int64_t end = 0;
  int64_t source_offset = -1;        // 源位置（字节偏移或帧号）
};

struct TrackSpec {
  std::string id;
  std::string role;                  // program / audio / subtitle / media
  int priority = 0;                  // 数值越大优先级越高
  int64_t boundary_start = 0;        // 轨允许的起止边界
  int64_t boundary_end = 0;           // 0 表示不设上界
  std::vector<Clip> clips;
};

struct Decision {
  std::string track_id;
  std::string code;
  std::string message;
  int64_t source_offset = -1;
};

struct PlacedSegment {
  std::string track_id;
  std::string source_ref;
  int64_t start = 0;
  int64_t end = 0;
  int priority = 0;
  int64_t source_offset = -1;
};

struct Timeline {
  std::vector<PlacedSegment> segments;  // 已按起点排序、互不重叠
  std::vector<Decision> decisions;
  int64_t start = 0;
  int64_t end = 0;
  size_t rejected = 0;
  size_t clipped = 0;
};

// 稳定裁定原因码。
inline constexpr const char* kTlNegative = "TL_NEGATIVE_START";
inline constexpr const char* kTlZeroLen = "TL_ZERO_LENGTH";
inline constexpr const char* kTlOutOfBounds = "TL_OUT_OF_BOUNDS";
inline constexpr const char* kTlEmptyTrack = "TL_EMPTY_TRACK";
inline constexpr const char* kTlOverlapLow = "TL_OVERLAP_LOW_PRIORITY";
inline constexpr const char* kTlOverlapTie = "TL_OVERLAP_TIE_SOURCE_ORDER";
inline constexpr const char* kTlTrackLimit = "TL_TOO_MANY_TRACKS";

Timeline compose_timeline(const std::vector<TrackSpec>& tracks,
                          int64_t global_end = 0);

}  // namespace mb
