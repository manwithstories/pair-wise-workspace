#pragma once
// 时钟修复：33 位 PTS 回绕展开、非法倒退识别、漂移估计。
#include <vector>
#include "media/packet.hpp"
#include "media/time_model.hpp"

namespace media {

struct RepairSummary {
  size_t packetsIn = 0;
  size_t packetsRepaired = 0; // 至少经过一次修复动作（回绕/重复/损坏标记）
  size_t wraps = 0;
  size_t duplicates = 0;
  size_t ambiguous = 0;
  bool deliverable = true;    // 出现无法安全修复的歧义时为 false
  size_t firstBadPacket = 0;  // 首个不可修复包位置
  DriftParams drift;
};

// 单轨流式修复器：逐包输入，内部维护回绕计数与上一时间戳。
class ClockRepair {
public:
  ClockRepair(Rational trackTimeBase, MediaType type);

  // 处理一个包（就地写入 repairedUs/durationUs/duplicate/deliverable）。
  // 返回该包上的事件；TimeError::None 表示无异常。
  TimeError accept(MediaPacket& pkt);

  // 音频轨按累计采样数喂入锚点（pkt 原始时间滴答, 应达微秒）。
  void addAnchor(double rawSeconds, double expectedSeconds);

  // 联合锚点估计漂移并钳制斜率；短样本/缺锚点 -> 低置信度。
  DriftParams estimateDrift();

  const RepairSummary& summary() const { return summary_; }
  const std::vector<TimeEvent>& events() const { return events_; }

private:
  Rational tb_;
  uint64_t wrapCount_ = 0;
  int64_t lastRaw_ = 0;     // 上一包展开后滴答
  int64_t lastMod_ = 0;     // 上一包 33 位模内滴答
  int64_t lastRepairedUs_ = 0;
  int64_t baseUs_ = 0;      // 首帧绝对时间，统一时间线据此归零
  bool haveLast_ = false;
  RepairSummary summary_;
  std::vector<TimeEvent> events_;
  std::vector<std::pair<double, double>> anchors_;
};

// 漂移后处理：把估计斜率应用到整轨（保持锚点 0 不动）。
void applyDrift(std::vector<MediaPacket>& packets, const DriftParams& d);
} // namespace media
