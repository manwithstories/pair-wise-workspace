#include "media/av_sync.hpp"

#include <algorithm>
#include <cmath>
#include <limits>

extern "C" {
#include <libswresample/swresample.h>
#include <libavutil/opt.h>
}

namespace media {

namespace {

std::int64_t percentile_95(std::vector<std::int64_t> values) {
  if (values.empty()) return 0;
  std::sort(values.begin(), values.end());
  const std::size_t idx =
      std::min(values.size() - 1, (values.size() * 95) / 100);
  return values[idx];
}

} // namespace

// Resampler context is created on demand per distinct source rate, so the
// public object stays small and FFmpeg types stay out of the header.
struct AVSync::Impl {
  SwrContext* swr = nullptr;
  std::uint32_t src_rate = 0;

  ~Impl() { if (swr) swr_free(&swr); }
};

AVSync::AVSync(const SyncConfig& cfg) : cfg_(cfg), impl_(std::make_unique<Impl>()) {}
AVSync::~AVSync() = default;

std::int64_t AVSync::normalize(std::int64_t raw, std::int64_t last_clock,
                               std::uint64_t index, bool missing,
                               std::int64_t expected_step_us) {
  // The jitter buffer hands us source-ordered, monotonic packets, so the raw
  // timestamp is trusted as the media clock. Correction never rewrites the
  // running baseline: a jump is only recorded (for the error model) and a
  // missing timestamp regenerated, which avoids cumulative drift.
  if (missing || raw == std::numeric_limits<std::int64_t>::min()) {
    const std::int64_t corrected = last_clock + expected_step_us;
    stats_.events.push_back(
        {ErrorCode::TimestampMissing, index, raw, corrected});
    ++stats_.timestamp_missing;
    return corrected;
  }
  if (index > 0) {
    const std::int64_t delta = raw - (last_clock + expected_step_us);
    if (delta > cfg_.resync_threshold_us ||
        -delta > cfg_.resync_threshold_us) {
      stats_.events.push_back(
          {ErrorCode::TimestampJump, index, raw, raw});
      ++stats_.timestamp_jumps;
    }
  }
  return raw;
}

std::int64_t AVSync::push_video(std::uint64_t frame_index, std::int64_t pts_us,
                                bool missing) {
  constexpr std::int64_t kFrameStepUs = 33'333;
  const std::int64_t normalized =
      normalize(pts_us, last_video_clock_, frame_index, missing, kFrameStepUs);
  last_video_clock_ = normalized;
  ++video_count_;
  return normalized;
}

SyncItem AVSync::push_audio(const AudioBlock& block, bool missing) {
  constexpr std::int64_t kBlockStepUs = 20'000; // 20 ms blocks
  const std::int64_t normalized =
      normalize(block.pts_us, last_audio_clock_, block.index, missing,
                kBlockStepUs);
  last_audio_clock_ = normalized;
  ++audio_count_;

  SyncItem item;
  item.kind = MediaKind::Audio;
  item.source_index = block.index;
  item.clock_us = normalized;
  item.raw_pts_us = block.pts_us;
  item.audio = block.samples;

  // Sample-rate switch: rebuild the resampler for the new source rate and
  // convert s16 mono PCM onto the unified target rate.
  if (last_sample_rate_ != 0 && block.sample_rate != last_sample_rate_) {
    stats_.events.push_back({ErrorCode::SampleRateChanged, block.index,
                             static_cast<std::int64_t>(last_sample_rate_),
                             static_cast<std::int64_t>(block.sample_rate)});
    ++stats_.sample_rate_changes;
  }
  if (block.sample_rate != cfg_.target_sample_rate) {
    if (impl_->src_rate != block.sample_rate) {
      if (impl_->swr) swr_free(&impl_->swr);
      AVChannelLayout mono = AV_CHANNEL_LAYOUT_MONO;
      int rv = swr_alloc_set_opts2(
          &impl_->swr,
          &mono, AV_SAMPLE_FMT_S16, cfg_.target_sample_rate,
          &mono, AV_SAMPLE_FMT_S16, block.sample_rate, 0, nullptr);
      if (rv == 0) swr_init(impl_->swr);
      impl_->src_rate = block.sample_rate;
    }
    if (impl_->swr) {
      // Output count follows the block duration on the target-rate clock:
      // a 20 ms block is target_rate/50 samples at any source rate.
      const int dst_capacity =
          static_cast<int>(cfg_.target_sample_rate / 50) + 32;
      std::vector<std::int16_t> converted(dst_capacity);
      const uint8_t* in = reinterpret_cast<const uint8_t*>(item.audio.data());
      uint8_t* out = reinterpret_cast<uint8_t*>(converted.data());
      int got = swr_convert(impl_->swr, &out, dst_capacity,
                            &in, static_cast<int>(item.audio.size()));
      if (got > 0) converted.resize(got);
      // Drain resampler delay so a fixed-size block keeps its exact duration.
      if (got >= 0 && got < dst_capacity) {
        uint8_t* tail = reinterpret_cast<uint8_t*>(
            converted.data() + std::max(got, 0));
        const int flushed = swr_convert(impl_->swr, &tail,
                                        dst_capacity - std::max(got, 0),
                                        nullptr, 0);
        if (flushed > 0) converted.resize(std::max(got, 0) + flushed);
      }
      item.audio = std::move(converted);
    }
  }
  last_sample_rate_ = block.sample_rate;

  // A/V error: distance from this audio clock position to the nearest video
  // frame grid point (33.333 ms spacing), which is the actual alignable error.
  if (video_count_ > 0) {
    constexpr std::int64_t kFrameStepUs = 33'333;
    const std::int64_t grid =
        std::llround(static_cast<double>(item.clock_us) / kFrameStepUs) *
        kFrameStepUs;
    stats_.abs_error_us.push_back(
        static_cast<std::int64_t>(std::llabs(item.clock_us - grid)));
  }
  return item;
}

std::vector<SyncItem> AVSync::merge(std::vector<SyncItem> audio_items,
                                    std::vector<SyncItem> video_items) {
  std::vector<SyncItem> all;
  all.reserve(audio_items.size() + video_items.size());
  for (auto& a : audio_items) all.push_back(std::move(a));
  for (auto& v : video_items) all.push_back(std::move(v));
  std::stable_sort(all.begin(), all.end(),
      [](const SyncItem& a, const SyncItem& b) {
        if (a.clock_us != b.clock_us) return a.clock_us < b.clock_us;
        return a.kind < b.kind; // video before audio on identical timestamps
      });
  return all;
}

void AVSync::finalize() {
  stats_.p95_error_us = percentile_95(stats_.abs_error_us);
}

} // namespace media
