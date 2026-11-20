#include <iomanip>
#include <sstream>

#include "mediacore/pipeline.hpp"

namespace mediacore {

using json::ObjectBuilder;
using json::Value;

namespace {

Value jRange(const TimeRange& r) {
    ObjectBuilder b;
    b.add("start", Value(r.start))
        .add("end", Value(r.end))
        .add("base_num", Value(r.base.num))
        .add("base_den", Value(r.base.den));
    return b.take();
}

Value jStream(const StreamInfo& s) {
    ObjectBuilder b;
    b.add("index", Value(s.index))
        .add("kind", Value(toString(s.kind)))
        .add("codec", Value(s.codecName))
        .add("profile", Value(s.profileName))
        .add("bitrate", Value(s.bitrate))
        .add("width", Value(s.width))
        .add("height", Value(s.height))
        .add("sample_rate", Value(s.sampleRate))
        .add("channels", Value(s.channels))
        .add("channel_layout", Value(s.channelLayout));
    return b.take();
}

Value jSegment(const SegmentAudit& s) {
    ObjectBuilder b;
    b.add("track", Value(s.track))
        .add("segment", Value(s.segment))
        .add("uri", Value(s.uri))
        .add("range", jRange(s.range))
        .add("sha256", Value(s.sha256))
        .add("keyframe_index_hit", Value(s.keyframeIndexHit))
        .add("keyframe_evaluated", Value(s.keyframeEvaluated))
        .add("corrupt", Value(s.corrupt))
        .add("skipped", Value(s.skipped));
    return b.take();
}

Value jPlan(const ResamplePlan& p) {
    Value::Array steps;
    for (const auto& s : p.steps)
        steps.push_back(ObjectBuilder{}
                            .add("action", Value(toString(s.action)))
                            .add("detail", Value(s.detail))
                            .take());
    return ObjectBuilder{}
        .add("feasible", Value(p.feasible))
        .add("layout_verified", Value(p.layoutVerified))
        .add("estimated_delay_samples",
             Value(p.estimatedDelaySamples))
        .add("source", Value(p.source.key()))
        .add("target", Value(p.target.key()))
        .add("steps", Value(std::move(steps)))
        .take();
}

Value jError(const MediaError& e) {
    return ObjectBuilder{}
        .add("code", Value(toString(e.code)))
        .add("recoverable", Value(e.recoverable))
        .add("track", Value(e.track))
        .add("segment", Value(e.segment))
        .add("uri", Value(e.uri))
        .add("detail", Value(e.detail))
        .take();
}

}  // namespace

json::Value AuditReport::toJson() const {
    Value::Array trackArr;
    for (const auto& t : tracks) {
        Value::Array segs, streams, plans;
        for (const auto& s : t.segments) segs.push_back(jSegment(s));
        for (const auto& s : t.probeStreams) streams.push_back(jStream(s));
        for (const auto& p : t.audioPlans) plans.push_back(jPlan(p));
        ObjectBuilder color;
        color.add("incomplete", Value(t.color.incomplete))
            .add("range",
                 Value(colorRangeName(t.color.resolved.range)))
            .add("primaries",
                 Value(colorPrimariesName(t.color.resolved.primaries)))
            .add("transfer",
                 Value(colorTransferName(t.color.resolved.transfer)))
            .add("matrix",
                 Value(colorMatrixName(t.color.resolved.matrix)));
        trackArr.push_back(ObjectBuilder{}
                               .add("track_id", Value(t.trackId))
                               .add("media_type", Value(t.mediaType))
                               .add("incomplete", Value(t.incomplete))
                               .add("segments", Value(std::move(segs)))
                               .add("streams", Value(std::move(streams)))
                               .add("audio_plans", Value(std::move(plans)))
                               .add("color", color.take())
                               .take());
    }
    Value::Array errArr;
    for (const auto& e : errors) errArr.push_back(jError(e));

    ObjectBuilder m;
    m.add("roundtrip_consistency", Value(metrics.roundtripConsistency))
        .add("keyframe_index_hit_rate",
             Value(metrics.keyframeIndexHitRate))
        .add("segment_sha256_count",
             Value(static_cast<int64_t>(metrics.segmentSha256Count)))
        .add("corrupt_ranges",
             Value(static_cast<int64_t>(metrics.corruptRanges)))
        .add("incomplete_tracks",
             Value(static_cast<int64_t>(metrics.incompleteTracks)))
        .add("total_segments",
             Value(static_cast<int64_t>(metrics.totalSegments)))
        .add("recovered_segments",
             Value(static_cast<int64_t>(metrics.recoveredSegments)));

    return ObjectBuilder{}
        .add("format", Value(toString(format)))
        .add("fatal", Value(fatal))
        .add("metrics", m.take())
        .add("tracks", Value(std::move(trackArr)))
        .add("errors", Value(std::move(errArr)))
        .take();
}

std::string AuditReport::toText() const {
    std::ostringstream ss;
    ss << std::fixed << std::setprecision(4);
    ss << "media_audit report (" << toString(format) << ")\n";
    ss << "  roundtrip_consistency=" << metrics.roundtripConsistency
       << "\n";
    ss << "  keyframe_index_hit_rate="
       << metrics.keyframeIndexHitRate << "\n";
    ss << "  segment_sha256_count=" << metrics.segmentSha256Count
       << "\n";
    ss << "  corrupt_ranges=" << metrics.corruptRanges << "\n";
    ss << "  incomplete_tracks=" << metrics.incompleteTracks << "\n";
    for (const auto& t : tracks) {
        ss << "  track " << t.trackId << " [" << t.mediaType << "]"
           << (t.incomplete ? " INCOMPLETE" : "") << " segments="
           << t.segments.size() << "\n";
        for (const auto& s : t.segments) {
            ss << "    #" << s.segment << " " << s.uri
               << " sha256=" << (s.sha256.empty() ? "-" : s.sha256)
               << " kf_hit=" << (s.keyframeIndexHit ? 1 : 0)
               << (s.corrupt ? " CORRUPT" : "")
               << (s.skipped ? " SKIPPED" : "") << "\n";
        }
    }
    for (const auto& e : errors)
        ss << "  ERROR " << toString(e.code) << " " << e.track << "#"
           << e.segment << ": " << e.detail << "\n";
    return ss.str();
}

}  // namespace mediacore
