#include <algorithm>
#include <filesystem>

#include "mediacore/timeline.hpp"

namespace mediacore {

namespace {

// Convert a manifest segment's start/duration onto the track's unified
// microsecond-ish base, refusing non-divisible mappings. `implicitStart`
// is the running cursor used by HLS segments without explicit ticks.
// SegmentRef stores duration as a reduced rational, so the raw tick count
// on the source scale is derived here instead of reusing numerator ticks.
bool placeOnBase(const SegmentRef& seg, int index,
                 const std::string& track, Rational unified,
                 Tick implicitStart, PlacedSegment& out,
                 ErrorLog& errors) {
    Rational srcBase = seg.tickBase;
    if (!seg.startTick) srcBase = Rational{1, 1};

    // Duration seconds = num/den. Rescale one second onto the unified
    // base, then multiply by the numerator; the first conversion must
    // already be exact so no rounding can hide here.
    Tick durTicks = 0;
    if (seg.duration.num != 0) {
        auto oneSec = scaleExact(1, Rational{1, 1}, unified);
        if (!oneSec || *oneSec % seg.duration.den != 0) {
            errors.add({ErrorCode::NonDivisibleScale, track, index,
                        seg.uri, {0, 0, unified}, true,
                        "segment duration does not map exactly to base"});
            return false;
        }
        durTicks = (*oneSec / seg.duration.den) * seg.duration.num;
    }

    Tick start = implicitStart;
    if (seg.startTick) {
        auto mapped = scaleExact(*seg.startTick, srcBase, unified);
        if (!mapped) {
            errors.add({ErrorCode::NonDivisibleScale, track, index,
                        seg.uri, {0, 0, unified}, true,
                        "segment start tick does not map exactly"});
            return false;
        }
        start = *mapped;
    }
    out.sequence = seg.sequence;
    out.uri = seg.uri;
    out.declaredDuration = seg.duration;
    out.sourceRange = TimeRange{
        seg.startTick.value_or(0),
        seg.startTick.value_or(0) + seg.duration.num, srcBase};
    out.range = TimeRange{start, start + durTicks, unified};
    out.zeroLength = durTicks == 0;
    return true;
}

}  // namespace

std::vector<TrackTimeline> buildTimelines(const Manifest& manifest,
                                          const TimelineOptions& opts,
                                          ErrorLog& errors) {
    std::vector<TrackTimeline> tracks;
    for (const auto& pl : manifest.playlists) {
        TrackTimeline tt;
        tt.trackId = pl.trackId.empty() ? pl.name : pl.trackId;
        tt.mediaType = pl.mediaType;
        tt.timeBase = opts.preferredBase;

        for (const auto& rep : pl.representations) {
            bool expectExplicit = false;
            for (const auto& s : rep.segments)
                if (s.startTick) expectExplicit = true;
            std::optional<Tick> prevEnd;
            std::optional<Tick> prevStart;

            for (std::size_t i = 0; i < rep.segments.size(); ++i) {
                const auto& seg = rep.segments[i];
                PlacedSegment placed;
                if (!placeOnBase(seg, static_cast<int>(i), tt.trackId,
                                 tt.timeBase, tt.cursor, placed, errors)) {
                    tt.incomplete = true;
                    continue;
                }
                if (placed.zeroLength) {
                    errors.add({ErrorCode::ZeroLengthRange, tt.trackId,
                                static_cast<int>(i), seg.uri, placed.range,
                                true, "zero-length segment"});
                    tt.incomplete = true;
                }
                if (expectExplicit && prevEnd) {
                    if (prevStart &&
                        placed.range.start == *prevStart) {
                        // Same start tick as the previous segment: a real
                        // duplicate, distinct from contiguous placement
                        // where this start equals the previous END.
                        errors.add({ErrorCode::DuplicateSegment,
                                    tt.trackId, static_cast<int>(i),
                                    seg.uri, placed.range, true,
                                    "segment repeats the previous start"});
                        tt.incomplete = true;
                    } else if (placed.range.start < *prevEnd) {
                        errors.add({ErrorCode::TimestampBackward,
                                    tt.trackId, static_cast<int>(i),
                                    seg.uri, placed.range, true,
                                    "segment start moves backward"});
                        tt.incomplete = true;
                    } else if (placed.range.start > *prevEnd) {
                        errors.add({ErrorCode::TimelineGap, tt.trackId,
                                    static_cast<int>(i), seg.uri,
                                    TimeRange{*prevEnd, placed.range.start,
                                              tt.timeBase},
                                    true, "gap between segments"});
                    }
                }
                prevStart = placed.range.start;
                prevEnd = placed.range.end;
                tt.cursor = placed.range.end;
                tt.segments.push_back(std::move(placed));
            }
        }
        if (!pl.ended) tt.incomplete = true;
        tracks.push_back(std::move(tt));
    }
    return tracks;
}

std::optional<Rational> unifiedTimebase(
    const std::vector<TrackTimeline>& tracks) {
    if (tracks.empty()) return TimelineOptions{}.preferredBase;
    Rational common = tracks.front().timeBase;
    for (std::size_t i = 1; i < tracks.size(); ++i) {
        auto next = commonTimebase(common, tracks[i].timeBase);
        if (!next) return std::nullopt;
        common = *next;
    }
    return common;
}

std::vector<CrossOverlap> findCrossTrackOverlaps(
    const std::vector<TrackTimeline>& tracks) {
    std::vector<CrossOverlap> out;
    for (std::size_t a = 0; a < tracks.size(); ++a) {
        for (std::size_t b = a + 1; b < tracks.size(); ++b) {
            for (std::size_t i = 0; i < tracks[a].segments.size(); ++i) {
                const auto& sa = tracks[a].segments[i].range;
                for (std::size_t j = 0;
                     j < tracks[b].segments.size(); ++j) {
                    const auto& sb = tracks[b].segments[j].range;
                    if (overlaps(sa, sb)) {
                        out.push_back(
                            CrossOverlap{tracks[a].trackId,
                                         tracks[b].trackId,
                                         static_cast<int>(i),
                                         static_cast<int>(j), sa});
                    }
                }
            }
        }
    }
    std::sort(out.begin(), out.end(), [](const auto& x, const auto& y) {
        if (x.trackA != y.trackA) return x.trackA < y.trackA;
        if (x.trackB != y.trackB) return x.trackB < y.trackB;
        if (x.segmentA != y.segmentA) return x.segmentA < y.segmentA;
        return x.segmentB < y.segmentB;
    });
    return out;
}

}  // namespace mediacore
