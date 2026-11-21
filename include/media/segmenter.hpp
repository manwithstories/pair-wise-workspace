#pragma once

#include <cstdint>
#include <vector>

#include "media/silence.hpp"
#include "media/time.hpp"

namespace media {

struct SegmenterConfig {
  SampleTick maxSamples = 0;          // 单片段最大样本数；0 表示不限
  SampleTick targetDurationTicks = 0; // 目标时长；0 表示不按时长切
  bool splitOnSilence = false;        // 是否在静音边界切开
};

struct Segment {
  int id = 0;                  // 从 0 开始的稳定编号
  SampleRange range;           // 半开区间，边界样本只归属一个片段
  bool containsGap = false;    // 区间内含损坏缺口（尾部截断等）
  int64_t durationMs = 0;
};

// 纯整数刻度的切分器：先给静音区间与总帧数规划边界，再流式派发音频块。
// 边界均为半开区间且首尾相接，保证每个样本最多归属一个片段。
class Segmenter {
 public:
  Segmenter(SegmenterConfig cfg, uint32_t sampleRate,
            std::vector<SilenceRange> silence, SampleTick totalFrames,
            std::vector<SampleRange> gaps = {});

  const std::vector<Segment>& segments() const { return segments_; }

  // 把一个音频块切给若干片段（回调按片段给出连续子块）。
  void dispatch(const AudioBlock& block,
                const std::function<void(const Segment&, const AudioBlock&)>&
                    onSegment) const;

 private:
  void planBoundaries();

  SegmenterConfig cfg_;
  uint32_t sampleRate_;
  std::vector<SilenceRange> silence_;
  std::vector<SampleRange> gaps_;
  SampleTick totalFrames_;
  std::vector<Segment> segments_;
};

}  // namespace media
