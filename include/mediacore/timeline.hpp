#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/manifest.hpp"
#include "mediacore/time.hpp"

namespace mediacore {

// One segment placed on a track's continuous integer-tick timeline.
struct PlacedSegment {
    int64_t sequence = 0;
    std::string uri;
    TimeRange range;                 // on the track's unified time base
    TimeRange sourceRange;           // ticks in the manifest's own scale
    Rational declaredDuration{0, 1};
    bool missing = false;
    bool zeroLength = false;
};

struct TrackTimeline {
    std::string trackId;
    std::string mediaType;
    Rational timeBase{1, 1};        // unified scale for this track
    Tick cursor = 0;                 // next expected start tick
    std::vector<PlacedSegment> segments;
    bool incomplete = false;        // recovery skipped a damaged window
};

struct TimelineOptions {
    // Preferred unified scale. Defaults to microseconds; cross-track joins
    // still require exact divisibility.
    Rational preferredBase{1, 1'000'000};
    bool strict = false;
};

// Place every manifest segment on its track's unified time base. All
// anomalies (backward/duplicate timestamps, gaps, zero-length ranges,
// non-divisible scale conversions) are emitted as located errors; nothing
// is silently rounded.
std::vector<TrackTimeline> buildTimelines(const Manifest& manifest,
                                          const TimelineOptions& opts,
                                          ErrorLog& errors);

// Common scale across every track, or nullopt if no exact common base
// exists (callers then keep per-track comparisons via compareTick).
std::optional<Rational> unifiedTimebase(
    const std::vector<TrackTimeline>& tracks);

// Find overlapping segments across tracks, used by the track graph to
// align interview / bcam / narration windows.
struct CrossOverlap {
    std::string trackA;
    std::string trackB;
    int segmentA;
    int segmentB;
    TimeRange range;  // expressed on trackA's base
};

std::vector<CrossOverlap> findCrossTrackOverlaps(
    const std::vector<TrackTimeline>& tracks);

}  // namespace mediacore
