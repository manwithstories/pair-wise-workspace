#include "timeline.hpp"

#include <algorithm>
#include <functional>
#include <map>
#include <set>
#include <string_view>

namespace media_restore {
namespace {

Diagnostic make(ErrorCode code, std::string message, const Location& location,
                Severity severity = Severity::error) {
  Diagnostic diagnostic;
  diagnostic.code = code;
  diagnostic.severity = severity;
  diagnostic.message = std::move(message);
  diagnostic.location = location;
  return diagnostic;
}

bool ticks(const Rational& time, const Rational& time_base, int64_t* out) {
  return Rational::exact_ticks(time, time_base, out);
}

}  // namespace

const TrackTimeline* Timeline::find(std::string_view track_id) const {
  for (const auto& track : tracks) {
    if (track.track_id == track_id) return &track;
  }
  return nullptr;
}

int64_t Timeline::total_ticks() const {
  int64_t total = 0;
  for (const auto& track : tracks) {
    if (!track.segments.empty()) {
      total = std::max(total, track.segments.back().end_tick);
    }
  }
  return total;
}

bool build_timeline(const Manifest& manifest,
                    std::vector<Diagnostic>* diagnostics, Timeline* out) {
  const size_t before = diagnostics->size();
  Timeline timeline;
  timeline.time_base = manifest.time_base;
  for (const auto& track : manifest.tracks) {
    TrackTimeline view;
    view.track_id = track.id;
    view.kind = track.kind;
    for (const auto& segment : track.segments) {
      Location loc;
      loc.track = track.id;
      loc.segment = segment.id;
      int64_t start = 0;
      int64_t duration = 0;
      if (!ticks(segment.start, manifest.time_base, &start)) {
        diagnostics->push_back(make(ErrorCode::manifest_schema,
                                    "segment start is not representable in the time base",
                                    loc));
        continue;
      }
      if (!ticks(segment.duration, manifest.time_base, &duration)) {
        diagnostics->push_back(make(ErrorCode::manifest_schema,
                                    "segment duration is not representable in the time base",
                                    loc));
        continue;
      }
      loc.tick = start;
      if (duration <= 0) {
        diagnostics->push_back(make(ErrorCode::segment_empty,
                                    "segment duration must be positive", loc));
        continue;
      }
      if (start < 0) {
        diagnostics->push_back(make(ErrorCode::segment_negative,
                                    "segment start must not be negative", loc));
        continue;
      }
      if (start > INT64_MAX - duration) {
        diagnostics->push_back(make(ErrorCode::limit_exceeded,
                                    "segment span overflows the time base", loc));
        continue;
      }
      TimelineSegment resolved;
      resolved.track_id = track.id;
      resolved.segment_id = segment.id;
      resolved.start_tick = start;
      resolved.duration_tick = duration;
      resolved.end_tick = start + duration;
      if (segment.audio) {
        resolved.has_audio = true;
        int64_t offset = 0;
        if (!ticks(segment.audio->offset, manifest.time_base, &offset)) {
          diagnostics->push_back(make(ErrorCode::manifest_schema,
                                      "audio offset is not representable in the time base",
                                      loc));
          offset = 0;
        } else if (offset < 0) {
          // The restore step prepends silence; record it deterministically.
          diagnostics->push_back(make(
              ErrorCode::segment_negative,
              "negative audio offset of " + std::to_string(offset) +
                  " ticks will trim the start of the source",
              loc, Severity::warning));
        }
        resolved.audio_offset_tick = offset;
      }
      view.segments.push_back(std::move(resolved));
    }

    // Overlap detection uses the exact integer spans, independent of the
    // order segments were declared in.
    std::vector<const TimelineSegment*> spans;
    spans.reserve(view.segments.size());
    for (const auto& segment : view.segments) spans.push_back(&segment);
    std::sort(spans.begin(), spans.end(),
              [](const TimelineSegment* a, const TimelineSegment* b) {
                if (a->start_tick != b->start_tick) return a->start_tick < b->start_tick;
                return a->segment_id < b->segment_id;
              });
    int64_t reach = -1;
    const TimelineSegment* reach_segment = nullptr;
    for (const TimelineSegment* span : spans) {
      if (reach_segment != nullptr && span->start_tick < reach) {
        Location loc;
        loc.track = track.id;
        loc.segment = span->segment_id;
        loc.tick = span->start_tick;
        diagnostics->push_back(make(
            ErrorCode::segment_overlap,
            "segment overlaps " + reach_segment->segment_id + " by " +
                std::to_string(reach - span->start_tick) + " ticks",
            loc));
      }
      if (span->end_tick > reach) {
        reach = span->end_tick;
        reach_segment = span;
      }
    }
    timeline.tracks.push_back(std::move(view));
  }

  // The graph is resolved even when spans failed, so the CLI can report both
  // structural and dependency problems in one pass.
  TrackOrder order;
  resolve_track_order(manifest, diagnostics, &order);
  timeline.order = std::move(order);
  *out = std::move(timeline);
  return diagnostics->size() == before;
}

bool resolve_track_order(const Manifest& manifest,
                         std::vector<Diagnostic>* diagnostics, TrackOrder* out) {
  const size_t before = diagnostics->size();
  std::map<std::string, size_t> index;
  for (size_t i = 0; i < manifest.tracks.size(); ++i) {
    index[manifest.tracks[i].id] = i;
  }

  // Kahn's algorithm over adjacency in manifest order for deterministic ties.
  std::vector<std::set<size_t>> outgoing(manifest.tracks.size());
  std::vector<size_t> indegree(manifest.tracks.size(), 0);
  std::vector<bool> missing_dependency(manifest.tracks.size(), false);
  for (size_t i = 0; i < manifest.tracks.size(); ++i) {
    const Track& track = manifest.tracks[i];
    for (const auto& dependency : track.depends_on) {
      const auto found = index.find(dependency);
      if (found == index.end()) {
        Location loc;
        loc.track = track.id;
        loc.field = "depends_on";
        diagnostics->push_back(make(
            ErrorCode::manifest_schema,
            "track depends on unknown track '" + dependency + "'", loc));
        missing_dependency[i] = true;
        continue;
      }
      if (found->second == i) {
        Location loc;
        loc.track = track.id;
        loc.field = "depends_on";
        diagnostics->push_back(make(ErrorCode::dependency_cycle,
                                    "track depends on itself", loc));
        missing_dependency[i] = true;
        continue;
      }
      if (outgoing[found->second].insert(i).second) ++indegree[i];
    }
  }

  std::vector<size_t> ready;
  for (size_t i = 0; i < indegree.size(); ++i) {
    if (indegree[i] == 0) ready.push_back(i);
  }
  std::sort(ready.begin(), ready.end());
  while (!ready.empty()) {
    const size_t current = ready.front();
    ready.erase(ready.begin());
    for (const size_t next : outgoing[current]) {
      if (--indegree[next] == 0) {
        ready.push_back(next);
        std::sort(ready.begin(), ready.end());
      }
    }
    out->order.push_back(manifest.tracks[current].id);
    if (missing_dependency[current]) {
      out->blocked.push_back(manifest.tracks[current].id);
    }
  }

  // Everything left has a non-zero indegree: it is on, or behind, a cycle.
  std::vector<std::string> cyclic;
  std::set<std::string> cycle_members;
  for (size_t i = 0; i < indegree.size(); ++i) {
    if (indegree[i] > 0) cyclic.push_back(manifest.tracks[i].id);
  }
  for (const auto& id : cyclic) {
    const size_t node = index[id];
    // A node is on the cycle itself if one of its dependencies is still
    // unresolved; otherwise it merely depends on the cycle.
    for (const auto& dependency : manifest.tracks[node].depends_on) {
      const auto found = index.find(dependency);
      if (found != index.end() && indegree[found->second] > 0) {
        cycle_members.insert(id);
        break;
      }
    }
  }
  for (const auto& id : cyclic) {
    Location loc;
    loc.track = id;
    diagnostics->push_back(make(
        ErrorCode::dependency_cycle,
        cycle_members.contains(id) ? "track participates in a dependency cycle"
                                   : "track depends on a cycle and cannot be ordered",
        loc));
    out->cyclic.push_back(id);
  }
  return diagnostics->size() == before;
}

bool build_timeline_with_order(const Manifest& manifest,
                               std::vector<Diagnostic>* diagnostics,
                               Timeline* out) {
  return build_timeline(manifest, diagnostics, out);
}

}  // namespace media_restore
