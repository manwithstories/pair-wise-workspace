#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "error.hpp"
#include "manifest.hpp"

namespace media_restore {

// A segment resolved to exact integer ticks of the manifest time base.  No
// floating point is involved, so overlapping boundaries are decided exactly.
struct TimelineSegment {
  std::string track_id;
  std::string segment_id;
  int64_t start_tick = 0;
  int64_t duration_tick = 0;
  int64_t end_tick = 0;
  int64_t audio_offset_tick = 0;  // negative values are compensated with silence
  bool has_audio = false;
};

struct TrackTimeline {
  std::string track_id;
  TrackKind kind = TrackKind::video;
  std::vector<TimelineSegment> segments;
};

struct TrackOrder {
  std::vector<std::string> order;   // restore order for acyclic tracks
  std::vector<std::string> cyclic;  // tracks involved in or behind a cycle
  std::vector<std::string> blocked; // tracks whose dependency failed or is missing
};

struct Timeline {
  Rational time_base{1, 90000};
  std::vector<TrackTimeline> tracks;
  TrackOrder order;

  const TrackTimeline* find(std::string_view track_id) const;
  int64_t total_ticks() const;  // end of the last segment across tracks
};

// Converts every segment to integer ticks and rejects empty, negative and
// overlapping spans.  Negative audio offsets produce warnings and are kept,
// because audio restoration compensates them with pad samples.
bool build_timeline(const Manifest& manifest,
                    std::vector<Diagnostic>* diagnostics, Timeline* out);

// Stable topological order: dependencies first, manifest order breaks ties.
// A cycle is reported with every involved track and does not stop acyclic
// tracks from being ordered.  Tracks that depend on a missing track are
// blocked and reported.
bool resolve_track_order(const Manifest& manifest,
                         std::vector<Diagnostic>* diagnostics, TrackOrder* out);

// Convenience: build_timeline(result order included).
bool build_timeline_with_order(const Manifest& manifest,
                               std::vector<Diagnostic>* diagnostics,
                               Timeline* out);

}  // namespace media_restore
