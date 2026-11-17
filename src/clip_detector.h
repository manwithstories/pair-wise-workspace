// clip_detector.h — 爆音/削波检测：绝对峰值、相邻斜率、局部恢复形态
//
// 检测分两层：
//   1) 每个声道独立扫描，产出同声道候选段，并把间隔极小的相邻段合并
//      （连续削波即使中间短暂跌落也只产生一个事件）；
//   2) 跨声道按帧索引合并关联事件，保留 channel_mask 记录来源声道。
//
// 正常方波（正负轨交替、快速恢复的高频大幅信号）通过"单轨性 +
// 平坦顶部 + 局部恢复形态"组合规则过滤，避免误报。
#pragma once

#include <cstddef>
#include <cstdint>
#include <vector>

#include "sample_model.h"

namespace popfix {

// 每个声道上的原始候选段（跨声道合并前的中间产物）。
struct RawSpan {
  std::size_t start = 0;
  std::size_t end = 0;  // 闭区间
  bool single_sample = false;
  bool positive_rail = true;
};

class ClipDetector {
 public:
  explicit ClipDetector(DetectorConfig cfg) : cfg_(cfg) {}

  // 单声道扫描：返回该声道全部候选段（已做同声道合并）。
  std::vector<RawSpan> detect_channel(const AudioBuffer& audio,
                                      std::uint8_t channel) const;

  // 完整检测：逐声道扫描后跨声道合并，事件按 start_frame 稳定排序，
  // event_id 从 0 顺序编号，channel_mask 标记参与声道。
  std::vector<ClipEvent> detect(const AudioBuffer& audio) const;

 private:
  // 局部恢复形态检查：候选段两侧的斜率必须显著大于信号局部常态斜率，
  // 且单点候选必须呈"尖峰"形态（两侧都向基线回落），用于滤除方波。
  bool passes_recovery_shape(const std::vector<double>& ch,
                             const RawSpan& span) const;
  // 同声道相邻段合并：间隔 <= merge_gap 且处于同一削波轨（同号）。
  static std::vector<RawSpan> merge_same_channel(std::vector<RawSpan> spans,
                                                 std::size_t gap);
  DetectorConfig cfg_;
};

}  // namespace popfix
