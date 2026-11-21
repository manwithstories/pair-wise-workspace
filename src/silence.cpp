#include "media/silence.hpp"

#include <algorithm>
#include <cmath>

namespace media {
namespace {
constexpr double kFullScale = 2147483648.0;
}

double AudioStats::rmsDb() const {
  return rms > 0.0 ? 20.0 * std::log10(rms / kFullScale) : -192.0;
}

double AudioStats::peakDb() const {
  return peak > 0 ? 20.0 * std::log10(static_cast<double>(peak) / kFullScale)
                  : -192.0;
}

SilenceDetector::SilenceDetector(SilenceConfig cfg, uint32_t sampleRate)
    : cfg_(cfg), sampleRate_(sampleRate) {}

void SilenceDetector::process(const AudioBlock& block) {
  for (size_t i = 0; i < block.frames; ++i) {
    const int32_t* frame = block.samples + i * block.channels;
    bool silent = true;
    for (int ch = 0; ch < block.channels; ++ch) {
      int32_t s = frame[ch];
      int64_t mag = s == INT32_MIN ? static_cast<int64_t>(INT32_MAX) + 1
                                   : std::abs(s);
      stats_.peak = std::max(stats_.peak, static_cast<int32_t>(
                                              std::min<int64_t>(mag, INT32_MAX)));
      sumSquares_ += static_cast<double>(s) * static_cast<double>(s);
      ++stats_.totalSamples;
      if (s == INT32_MIN || s == INT32_MAX) ++stats_.clippedSamples;
      if (mag > cfg_.threshold) silent = false;
    }
    SampleTick tick = block.firstSample + static_cast<SampleTick>(i);
    if (silent) {
      if (runStart_ < 0) runStart_ = tick;
      ++stats_.silentFrames;
    } else if (runStart_ >= 0) {
      closeRange(tick);
    }
  }
}

void SilenceDetector::closeRange(SampleTick end) {
  SampleTick length = end - runStart_;
  if (length >= cfg_.minDurationTicks) {
    ranges_.push_back(SilenceRange{
        SampleRange{runStart_, end}, ticksToMs(length, sampleRate_)});
  }
  runStart_ = -1;
}

std::vector<SilenceRange> SilenceDetector::finish(SampleTick totalFrames) {
  if (runStart_ >= 0) closeRange(totalFrames);
  if (stats_.totalSamples > 0)
    stats_.rms = std::sqrt(sumSquares_ / static_cast<double>(
                                             stats_.totalSamples));
  return ranges_;
}

}  // namespace media
