#include "media/segmenter.hpp"

#include <algorithm>
#include <functional>

namespace media {

Segmenter::Segmenter(SegmenterConfig cfg, uint32_t sampleRate,
                     std::vector<SilenceRange> silence,
                     SampleTick totalFrames,
                     std::vector<SampleRange> gaps)
    : cfg_(cfg),
      sampleRate_(sampleRate),
      silence_(std::move(silence)),
      gaps_(std::move(gaps)),
      totalFrames_(totalFrames) {
  planBoundaries();
}

void Segmenter::planBoundaries() {
  // 候选切点：静音区间的起点（splitOnSilence 时）与目标时长网格。
  std::vector<SampleTick> cuts;
  if (cfg_.splitOnSilence) {
    for (const auto& s : silence_) {
      if (s.range.start > 0) cuts.push_back(s.range.start);
      if (s.range.end < totalFrames_) cuts.push_back(s.range.end);
    }
  }
  if (cfg_.targetDurationTicks > 0) {
    for (SampleTick t = cfg_.targetDurationTicks; t < totalFrames_;
         t += cfg_.targetDurationTicks)
      cuts.push_back(t);
  }
  std::sort(cuts.begin(), cuts.end());
  cuts.erase(std::unique(cuts.begin(), cuts.end()), cuts.end());

  // 以 maxSamples 为硬上限：超限区间在最近的候选切点断开，否则强制断开。
  SampleTick start = 0;
  auto makeSegment = [&](SampleTick end) {
    Segment seg;
    seg.id = static_cast<int>(segments_.size());
    seg.range = SampleRange{start, end};
    seg.durationMs = ticksToMs(end - start, sampleRate_);
    for (const auto& g : gaps_) {
      if (g.start < end && g.end > start) {
        seg.containsGap = true;
        break;
      }
    }
    segments_.push_back(seg);
    start = end;
  };

  size_t cutIdx = 0;
  while (start < totalFrames_) {
    SampleTick hardEnd =
        cfg_.maxSamples > 0 ? start + cfg_.maxSamples : totalFrames_;
    SampleTick chosen = totalFrames_;
    while (cutIdx < cuts.size() && cuts[cutIdx] <= start) ++cutIdx;
    for (size_t i = cutIdx; i < cuts.size() && cuts[i] < hardEnd; ++i)
      chosen = cuts[i];  // 取硬上限内最后一个候选切点
    if (chosen == totalFrames_ || chosen <= start) {
      chosen = std::min(hardEnd, totalFrames_);
    } else {
      cutIdx = std::upper_bound(cuts.begin(), cuts.end(), chosen) -
               cuts.begin();
    }
    makeSegment(chosen);
  }
}

void Segmenter::dispatch(
    const AudioBlock& block,
    const std::function<void(const Segment&, const AudioBlock&)>& onSegment)
    const {
  for (const Segment& seg : segments_) {
    SampleTick lo = std::max(seg.range.start, block.firstSample);
    SampleTick hi = std::min(seg.range.end,
                             block.firstSample + static_cast<SampleTick>(block.frames));
    if (hi <= lo) continue;
    size_t offset = static_cast<size_t>(lo - block.firstSample);
    AudioBlock sub{block.samples + offset * block.channels,
                   static_cast<size_t>(hi - lo), block.channels, lo};
    onSegment(seg, sub);
  }
}

}  // namespace media
