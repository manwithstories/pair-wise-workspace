// Audio/video synchronization on one virtual media clock (microseconds).
// Video frames and audio blocks are aligned by presentation timestamp; the
// module has deterministic policies for missing timestamps, sudden jumps and
// audio sample-rate changes (resampled through libswresample).
#pragma once

#include <cstdint>
#include <memory>
#include <vector>

#include "media/packet.hpp"

namespace media {

struct SyncConfig {
  std::int64_t resync_threshold_us = 100'000; // jump beyond this re-clocks
  std::uint32_t target_sample_rate = 48'000;  // unified audio rate
};

struct SyncEvent {
  ErrorCode code = ErrorCode::None;
  std::uint64_t index = 0;     // frame or audio block index
  std::int64_t observed_us = 0;
  std::int64_t corrected_us = 0;
};

struct SyncStats {
  std::vector<SyncEvent> events;
  std::vector<std::int64_t> abs_error_us; // |audio_pts - video_pts| pairs
  std::uint64_t timestamp_missing = 0;
  std::uint64_t timestamp_jumps = 0;
  std::uint64_t sample_rate_changes = 0;
  std::int64_t p95_error_us = 0;
};

// One aligned timeline item emitted by the synchronizer.
struct SyncItem {
  MediaKind kind = MediaKind::Video;
  std::uint64_t source_index = 0;
  std::int64_t clock_us = 0;     // normalized time on the unified clock
  std::int64_t raw_pts_us = 0;
  std::vector<std::int16_t> audio; // present for audio items, post-resample
};

class AVSync {
 public:
  explicit AVSync(const SyncConfig& cfg = {});
  ~AVSync();

  // Normalize one video frame timestamp onto the media clock.
  std::int64_t push_video(std::uint64_t frame_index, std::int64_t pts_us,
                          bool missing);

  // Normalize an audio block and, when its rate differs from the target,
  // resample its PCM through libswresample to the unified sample rate.
  SyncItem push_audio(const AudioBlock& block, bool missing);

  // Merge normalized video and audio items into one clock-ordered timeline.
  std::vector<SyncItem> merge(std::vector<SyncItem> audio_items,
                              std::vector<SyncItem> video_items);

  const SyncStats& stats() const noexcept { return stats_; }
  void finalize(); // computes p95_error_us from collected pairs

 private:
  struct Impl;
  SyncConfig cfg_;
  SyncStats stats_;
  std::unique_ptr<Impl> impl_;
  std::int64_t last_video_clock_ = 0;
  std::int64_t last_audio_clock_ = 0;
  std::uint32_t last_sample_rate_ = 0;
  std::uint64_t video_count_ = 0;
  std::uint64_t audio_count_ = 0;

  std::int64_t normalize(std::int64_t raw, std::int64_t last,
                         std::uint64_t index, bool missing,
                         std::int64_t expected_step_us);
};

} // namespace media
