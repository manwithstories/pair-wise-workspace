#include "media/fingerprint.hpp"

#include <algorithm>

namespace media {
namespace {

// 多声道规范化为单声道混合（求和后按声道数缩放，保持满幅刻度）。
int32_t downmixFrame(const int32_t* frame, int channels) {
  int64_t acc = 0;
  for (int ch = 0; ch < channels; ++ch) acc += frame[ch];
  return static_cast<int32_t>(acc / channels);
}

}  // namespace

Fingerprinter::Fingerprinter(FingerprintConfig cfg) : cfg_(cfg) {
  if (cfg_.windowTicks <= 0) cfg_.windowTicks = 1024;
}

SegmentFingerprint Fingerprinter::build(
    const Segment& seg, const std::vector<int32_t>& interleaved,
    int channels) const {
  SegmentFingerprint fp;
  fp.segmentId = seg.id;
  fp.range = seg.range;
  SampleTick frames =
      seg.range.length();  // 调用方保证传入样本与区间长度一致
  FnvHash combo;
  SampleTick pos = 0;
  while (pos < frames) {
    SampleTick n = std::min(cfg_.windowTicks, frames - pos);
    FnvHash window;
    for (SampleTick i = 0; i < n; ++i) {
      int32_t mono =
          downmixFrame(interleaved.data() + (pos + i) * channels, channels);
      window.update(&mono, sizeof(mono));
    }
    uint64_t h = window.value();
    fp.windowHashes.push_back(h);
    combo.update(&h, sizeof(h));
    pos += n;
  }
  fp.signature = combo.value();
  return fp;
}

bool Fingerprinter::findOrRegister(const SegmentFingerprint& fp,
                                   DuplicateMatch* match) {
  for (const auto& prior : known_) {
    // 快速过滤：窗口数差异过大不可能达到阈值。
    size_t lo = std::min(fp.windowHashes.size(), prior.windowHashes.size());
    size_t hi = std::max(fp.windowHashes.size(), prior.windowHashes.size());
    if (hi == 0 || static_cast<double>(lo) / static_cast<double>(hi) <
                        cfg_.matchThreshold)
      continue;
    size_t common = 0;
    for (size_t i = 0; i < lo; ++i)
      if (fp.windowHashes[i] == prior.windowHashes[i]) ++common;
    double score =
        static_cast<double>(common) / static_cast<double>(fp.windowHashes.size());
    if (score >= cfg_.matchThreshold) {
      if (match) {
        match->segmentId = fp.segmentId;
        match->firstSourceId = prior.segmentId;
        match->sourceRange = prior.range;
        match->score = score;
      }
      return true;
    }
  }
  known_.push_back(fp);
  return false;
}

}  // namespace media
