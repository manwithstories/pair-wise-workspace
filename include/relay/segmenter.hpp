#pragma once

#include <cstdint>
#include <functional>
#include <string>
#include <vector>

#include "relay/timeline.hpp"
#include "relay/types.hpp"

namespace relay {

enum class SegmentStatus {
  kComplete,      ///< start/end covered by verified frames, seekable
  kTailTruncated, ///< last segment shorter than target, explainable tail
  kGap,           ///< a marked delivery gap intersects the segment
  kUnseekable,    ///< no keyframe index possible for this segment
};

struct Segment {
  TrackId track;
  std::uint64_t index = 0;
  Micros start_us = 0;
  Micros end_us = 0;
  std::uint32_t start_sequence = 0;
  std::uint32_t end_sequence = 0;
  std::size_t frame_count = 0;
  SegmentStatus status = SegmentStatus::kComplete;
  /// First keyframe at or after start_us; 0 when absent.
  std::uint32_t keyframe_sequence = 0;
  bool random_access = false;
  std::string status_reason;
  std::string content_hash;  // SHA-256 over the segment's frame identities
};

struct SegmentOptions {
  Micros target_duration_us = 2'000'000;
  /// Allowed deviation between the chosen cut and the target; must not
  /// exceed one source frame.
  Micros max_boundary_error_us = kLongFrameDurationUs;
};

struct TrackSegments {
  TrackId track;
  Timebase timebase;
  std::vector<Segment> segments;
  std::vector<Segment> tail_dropped;  // consumed but unrepresentable
  std::size_t unseekable = 0;
};

/// Splits a track on keyframes close to the target duration.  Cut points
/// prefer the keyframe nearest the target; the tail always yields an
/// explainable final segment instead of being silently discarded.
TrackSegments segment_track(const TimelineTrack& track,
                            const SegmentOptions& options, ErrorSink& errors);

}  // namespace relay
