#include "media/clock_repair.hpp"
#include <algorithm>
#include <cmath>

namespace media {
namespace {
// 回绕判定：回退幅度超过半模视为 33 位回绕；小于该阈值的回退为非法倒退。
constexpr uint64_t WRAP_THRESHOLD = PTS33_MOD / 2;
// 漂移估计最少锚点数与最小时间跨度（秒）。
constexpr size_t MIN_ANCHORS = 8;
constexpr double MIN_ANCHOR_SPAN_S = 0.5;
} // namespace

ClockRepair::ClockRepair(Rational tb, MediaType) : tb_(tb) {}

TimeError ClockRepair::accept(MediaPacket& pkt) {
  summary_.packetsIn++;
  TimeError err = TimeError::None;
  uint64_t raw = uint64_t(pkt.rawPts) & PTS33_MASK;

  if (haveLast_) {
    // 33 位模空间下的前向增量（始终落在 [0, PTS33_MOD)）。
    uint64_t fwd = (raw - uint64_t(lastMod_)) & PTS33_MASK;
    if (fwd == 0) {
      // 重复 PTS：可恢复，保留并标记。
      err = TimeError::DuplicatePts;
      pkt.duplicate = true;
      summary_.duplicates++;
      summary_.packetsRepaired++;
    } else if (fwd >= WRAP_THRESHOLD) {
      // 前向增量超过半模，等价于小幅非法倒退，无法安全修复。
      err = TimeError::Ambiguous;
      summary_.ambiguous++;
      summary_.deliverable = false;
      if (summary_.firstBadPacket == 0)
        summary_.firstBadPacket = pkt.globalIndex;
      pkt.deliverable = false;
    } else if (raw < uint64_t(lastMod_)) {
      // 数值回退但前向增量正常 -> 合法 33 位回绕。
      wrapCount_++;
      err = TimeError::Wrap;
      summary_.wraps++;
      summary_.packetsRepaired++;
    }
  }

  const uint64_t unwrapped = raw + wrapCount_ * PTS33_MOD;
  // 展开后仍倒退（回绕幅度与模数不自洽）-> 非法倒退。
  if (haveLast_ && int64_t(unwrapped) <= lastRaw_ &&
      err != TimeError::DuplicatePts && err != TimeError::Ambiguous) {
    err = TimeError::BackwardIllegal;
    summary_.deliverable = false;
    pkt.deliverable = false;
    if (summary_.firstBadPacket == 0)
      summary_.firstBadPacket = pkt.globalIndex;
  }

  pkt.repairedUs = ticksToUs(int64_t(unwrapped), tb_);
  pkt.durationUs = ticksToUs(pkt.durationTicks, tb_);
  // 统一时间线按首帧归零，消除各设备不同的绝对起点（如接近 2^33）。
  if (!haveLast_) baseUs_ = pkt.repairedUs;
  pkt.repairedUs -= baseUs_;
  if (pkt.duplicate && haveLast_) pkt.repairedUs = lastRepairedUs_;

  if (err != TimeError::None)
    events_.push_back({pkt.globalIndex, pkt.rawPts, pkt.repairedUs, err,
                       std::string(toString(err))});
  lastRaw_ = int64_t(unwrapped);
  lastMod_ = int64_t(raw);
  lastRepairedUs_ = pkt.repairedUs;
  haveLast_ = true;
  return err;
}

void ClockRepair::addAnchor(double rawSeconds, double expectedSeconds) {
  anchors_.push_back({rawSeconds, expectedSeconds});
}

DriftParams ClockRepair::estimateDrift() {
  DriftParams& d = summary_.drift;
  if (anchors_.size() < MIN_ANCHORS) {
    d.lowConfidence = true;
    d.confidence = 0.0;
    events_.push_back({summary_.packetsIn, 0, 0, TimeError::MissingAnchor,
                       "insufficient anchors: " +
                           std::to_string(anchors_.size())});
    return d;
  }
  double span = anchors_.back().first - anchors_.front().first;
  auto fit = fitLine(anchors_);
  if (!fit.valid || span < MIN_ANCHOR_SPAN_S) {
    d.lowConfidence = true;
    d.confidence = 0.0;
    events_.push_back({summary_.packetsIn, 0, 0, TimeError::MissingAnchor,
                       "anchor span too short"});
    return d;
  }
  // 锚点约定 (x=参考时间, y=设备实测时间)，故 fit.a = 实测/参考 = 1+漂移。
  // 正 ppm 表示设备时钟偏快；校正斜率取倒数，且受安全斜率上限钳制。
  double ppm = clampSlopePpm((fit.a - 1.0) * 1e6);
  d.driftPpm = ppm;
  d.slope = 1.0 / (1.0 + clampSlopePpm(ppm) / 1e6);
  d.r2 = fit.r2;
  d.confidence = std::clamp(fit.r2, 0.0, 1.0);
  // 样本量不足或拟合离散时降级置信度。
  d.lowConfidence = d.confidence < 0.9 || anchors_.size() < MIN_ANCHORS * 2;
  return d;
}

void applyDrift(std::vector<MediaPacket>& packets, const DriftParams& d) {
  if (packets.empty()) return;
  const int64_t t0 = packets.front().repairedUs;
  for (auto& p : packets) {
    p.repairedUs = t0 + int64_t(double(p.repairedUs - t0) * d.slope) +
                   d.offsetUs;
    p.durationUs = int64_t(double(p.durationUs) * d.slope);
  }
}
} // namespace media
