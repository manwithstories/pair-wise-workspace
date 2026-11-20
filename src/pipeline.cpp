#include <algorithm>
#include <filesystem>

#include "mediacore/pathutil.hpp"
#include "mediacore/pipeline.hpp"
#include "mediacore/sha256.hpp"
#include "mediacore/track_graph.hpp"

namespace mediacore {

namespace fs = std::filesystem;

namespace {

bool isFatalNow(const ErrorLog& log, bool strict) {
    return strict && log.hasFatal();
}

// Resolve a segment URI against the manifest directory, constrained to
// the configured input root.
fs::path resolve(const Manifest& m, const std::string& uri) {
    const auto dir = m.sourcePath.parent_path();
    auto p = resolveManifestUri(dir, uri, dir);
    return p.value_or(dir / uri);
}

}  // namespace

AuditReport runPipeline(const Manifest& manifest,
                        const std::vector<TrackSpec>& specs,
                        const AuditOptions& opts, ErrorLog& errors) {
    AuditReport report;
    report.format = manifest.format;

    // Stage 1: round-trip consistency over retained manifest fields.
    const auto rt = roundTripCompare(manifest);
    report.metrics.roundtripConsistency = rt.consistency();

    // Stage 2: exact multi-track timeline.
    TimelineOptions tlOpts;
    tlOpts.strict = opts.strict;
    auto timelines = buildTimelines(manifest, tlOpts, errors);

    // Stage 3/4/5 run per track through the dependency graph so unrelated
    // tracks can analyze concurrently while ordering stays deterministic.
    std::vector<std::string> trackIds;
    for (const auto& t : timelines) trackIds.push_back(t.trackId);
    auto graph = buildStandardGraph(trackIds, errors);
    auto order = graph.topologicalOrder(errors);
    if (!order) {
        report.fatal = true;
        report.errors = errors.items();
        return report;
    }

    report.tracks.reserve(timelines.size());
    for (std::size_t ti = 0; ti < timelines.size(); ++ti) {
        const auto& tl = timelines[ti];
        TrackAudit ta;
        ta.trackId = tl.trackId;
        ta.mediaType = tl.mediaType;
        ta.incomplete = tl.incomplete;

        std::vector<AudioSpec> audioSpecs;
        std::vector<ColorMetadata> colorSpecs;
        std::vector<SegmentProbe> probes;
        probes.reserve(tl.segments.size());

        for (std::size_t si = 0; si < tl.segments.size(); ++si) {
            const auto& placed = tl.segments[si];
            const fs::path path = resolve(manifest, placed.uri);

            SegmentAudit sa;
            sa.track = tl.trackId;
            sa.segment = static_cast<int>(si);
            sa.uri = placed.uri;
            sa.range = placed.range;

            if (isFatalNow(errors, opts.strict)) {
                report.fatal = true;
                break;
            }

            ProbeOptions popts;
            popts.maxPacketsPerSegment = opts.maxPacketsPerSegment;
            popts.declaredDuration = placed.declaredDuration;
            auto probe = probeSegment(path, tl.trackId,
                                      static_cast<int>(si), popts, errors);
            probes.push_back(probe);
            sa.corrupt = probe.tailCorrupt || !probe.opened;

            if (sa.corrupt && opts.recover) {
                sa.skipped = true;
                ta.incomplete = true;
                ++report.metrics.recoveredSegments;
            }

            for (const auto& st : probe.streams) {
                ta.probeStreams.push_back(st);
                if (st.kind == MediaKind::Audio)
                    audioSpecs.push_back(
                        AudioSpec{st.sampleRate, st.channels,
                                  st.channelLayout, AV_SAMPLE_FMT_FLTP});
                if (st.kind == MediaKind::Video)
                    colorSpecs.push_back(ColorMetadata{
                        st.colorRange, st.colorPrimaries,
                        st.colorTransfer, st.colorSpace});
            }

            if (!sa.corrupt || !sa.skipped) {
                if (auto digest = hashFile(path)) {
                    sa.sha256 = *digest;
                    ++report.metrics.segmentSha256Count;
                }
            }
            ta.segments.push_back(std::move(sa));
            if (report.fatal) break;
        }

        // Keyframe index hit evaluation. Each segment is a standalone
        // file whose PTS restarts at the local origin, so evaluate against
        // zero-start boundaries on each probe's own time base.
        std::vector<TimeRange> localBoundaries;
        localBoundaries.reserve(probes.size());
        for (const auto& p : probes)
            localBoundaries.push_back(
                TimeRange{0, 1, p.keyframeBase});
        auto hits = evaluateKeyframeIndex(probes, localBoundaries);
        report.metrics.keyframeIndexHitRate = hits.hitRate();
        for (std::size_t i = 0; i < ta.segments.size() &&
                               i < probes.size(); ++i) {
            ta.segments[i].keyframeEvaluated =
                probes[i].opened && !probes[i].keyframes.empty();
            if (i < hits.evaluated) {
                // Standalone files restart at the local origin; accept a
                // first keyframe within one frame of PTS zero.
                bool hit = false;
                if (probes[i].opened && !probes[i].keyframes.empty()) {
                    const auto first = probes[i].keyframes[0].pts;
                    Tick tol = 0;
                    for (const auto& st : probes[i].streams) {
                        if (st.kind == MediaKind::Video &&
                            st.frameRate.num > 0) {
                            auto oneFrame = scaleExact(
                                1, st.frameRate,
                                probes[i].keyframes[0].base);
                            if (oneFrame) tol = *oneFrame;
                            break;
                        }
                    }
                    hit = first <= tol;
                }
                ta.segments[i].keyframeIndexHit = hit;
            }
        }

        // Audio normalization planning against the requested target.
        detectAudioConflicts(tl.trackId, audioSpecs, errors);
        for (std::size_t i = 0; i < audioSpecs.size(); ++i) {
            ta.audioPlans.push_back(planAudioNormalization(
                tl.trackId, static_cast<int>(i), audioSpecs[i],
                opts.audioTarget, errors));
        }

        // Color metadata normalization under the CLI policy.
        if (!colorSpecs.empty())
            ta.color = checkColorMetadata(tl.trackId, colorSpecs,
                                          opts.colorPolicy, errors);
        else
            ta.color.incomplete = true;

        report.tracks.push_back(std::move(ta));
        if (report.fatal) break;
    }

    // Aggregate metrics from the final error log and track states.
    report.metrics.totalSegments = 0;
    report.metrics.corruptRanges = errors.count(ErrorCode::SegmentCorrupt);
    report.metrics.corruptRanges +=
        errors.count(ErrorCode::ZeroLengthRange);
    for (const auto& t : report.tracks) {
        report.metrics.totalSegments += t.segments.size();
        if (t.incomplete) ++report.metrics.incompleteTracks;
    }
    report.errors = errors.items();
    report.fatal = report.fatal || errors.hasFatal();
    return report;
}

}  // namespace mediacore
