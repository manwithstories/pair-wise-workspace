#pragma once

#include <map>
#include <optional>
#include <string>
#include <vector>

#include "relay/packet_reorder.hpp"
#include "relay/types.hpp"

namespace relay {

enum class TrackState { kReady, kBlocked, kEmpty };

struct TrackNode {
  TrackSpec spec;
  TrackState state = TrackState::kReady;
  /// Dependencies that failed or were missing, in graph order.
  std::vector<TrackId> failed_dependencies;
  std::vector<TrackId> empty_dependencies;
  /// Human-readable chain such as "segment<-timeline<-cam-b".
  std::string dependency_chain;
};

struct TimelineTrack {
  TrackId id;
  std::vector<FrameRecord> frames;  // epoch-local pts_us preserved
  std::vector<LostRange> losses;
  std::vector<std::uint32_t> keyframe_sequences;
  Timebase timebase;
  int epochs = 0;
  ReorderStats stats;
};

/// The explicit, global time axis shared by every representation layer.
struct Timeline {
  Timebase timebase;
  Micros start_us = 0;
  Micros end_us = 0;
  std::vector<TimelineTrack> tracks;
  std::vector<TrackNode> graph;  // topological order
  std::vector<TrackId> empty_tracks;
  std::vector<TrackId> blocked_tracks;
};

/// Builds the timeline and the dependency graph.  Resolution is explicit:
/// processing order is the topological order of `TrackSpec::depends_on`,
/// and a track whose dependency is empty or blocked is itself blocked
/// rather than being silently processed out of order.
Timeline build_timeline(const std::vector<TrackInput>& inputs,
                        const std::vector<TrackStream>& streams,
                        ErrorSink& errors);

/// True when the node has no failed/empty dependency and its own state is
/// ready.  Callers must use this before consuming a track's output.
[[nodiscard]] bool is_usable(const TrackNode& node);

[[nodiscard]] const TrackNode* find_node(const Timeline& timeline,
                                         const TrackId& id);

/// All frames of `track` on the global axis (epoch offsets accumulated).
[[nodiscard]] std::vector<FrameRecord> global_frames(const Timeline& timeline,
                                                     const TimelineTrack& track);

}  // namespace relay
