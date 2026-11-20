#pragma once

#include <cstddef>
#include <cstdint>
#include <deque>
#include <map>
#include <optional>
#include <string>
#include <vector>

#include "relay/frame_reader.hpp"
#include "relay/types.hpp"

namespace relay {

struct ReorderStats {
  std::size_t received = 0;
  std::size_t duplicates = 0;
  std::size_t reordered = 0;
  std::size_t late_dropped = 0;
  std::size_t lost_marked = 0;
  std::size_t epochs = 1;
  std::size_t unverified_quarantined = 0;
  std::size_t peak_cache_bytes = 0;
};

/// One contiguous stretch of frames sharing a monotonic timestamp mapping.
/// A timestamp wrap or sequence reset starts a new epoch so that no two
/// unrelated captures are silently spliced together.
struct TimelineEpoch {
  int index = 0;
  std::string cause;
  Micros origin_us = 0;
  std::vector<FrameRecord> frames;   // sorted by pts_us
  std::vector<LostRange> losses;
  std::uint32_t first_sequence = 0;
  std::size_t duplicates = 0;
  std::size_t late_dropped = 0;
};

struct TrackStream {
  TrackId track;
  Timebase timebase;
  std::vector<TimelineEpoch> epochs;
  ReorderStats stats;

  [[nodiscard]] bool empty() const;
  [[nodiscard]] std::size_t frame_count() const;
  [[nodiscard]] std::vector<LostRange> all_losses() const;
  [[nodiscard]] std::optional<Micros> first_pts() const;
  [[nodiscard]] std::optional<Micros> last_pts() const;
};

/// Reorders an arrival stream by (unwrapped timestamp, sequence), drops
/// duplicates, marks gaps per track policy and splits epochs on
/// timestamp wrap / sequence reset.  It never re-emits a frame that was
/// already delivered.
TrackStream reorder_track(const TrackInput& input, ErrorSink& errors);

}  // namespace relay
