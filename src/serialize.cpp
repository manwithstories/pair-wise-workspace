#include "mediacore/json.hpp"
#include "mediacore/manifest.hpp"

namespace mediacore {

using json::ObjectBuilder;
using json::Value;

namespace {

Value toJson(const Rational& r) {
    ObjectBuilder b;
    b.add("num", Value(r.num)).add("den", Value(r.den));
    return b.take();
}

Value toJson(const SegmentRef& s) {
    ObjectBuilder b;
    b.add("sequence", Value(s.sequence))
        .add("discontinuity", Value(s.discontinuity))
        .add("duration", toJson(s.duration))
        .add("uri", Value(s.uri))
        .add("init_uri", Value(s.initUri))
        .add("byterange", Value(s.byterange))
        .add("independent", Value(s.independent));
    if (s.startTick) {
        b.add("start_tick", Value(*s.startTick));
        b.add("tick_base", toJson(s.tickBase));
    }
    return b.take();
}

Value toJson(const Representation& r) {
    Value::Array codecs;
    for (const auto& c : r.codecs) codecs.emplace_back(c);
    Value::Array segments;
    for (const auto& s : r.segments) segments.push_back(toJson(s));
    ObjectBuilder b;
    b.add("id", Value(r.id))
        .add("content_type", Value(r.contentType))
        .add("bandwidth_bps", Value(r.bandwidthBps))
        .add("codecs", Value(std::move(codecs)))
        .add("lang", Value(r.lang))
        .add("frame_rate", toJson(r.frameRate))
        .add("width", Value(r.width))
        .add("height", Value(r.height))
        .add("segments", Value(std::move(segments)));
    return b.take();
}

Value toJson(const MediaPlaylist& p) {
    Value::Array reps;
    for (const auto& r : p.representations) reps.push_back(toJson(r));
    ObjectBuilder b;
    b.add("track_id", Value(p.trackId))
        .add("name", Value(p.name))
        .add("uri", Value(p.uri))
        .add("media_type", Value(p.mediaType))
        .add("target_duration", toJson(p.targetDuration))
        .add("ended", Value(p.ended))
        .add("independent_segments", Value(p.independentSegments));
    if (p.programStart) b.add("program_start", Value(*p.programStart));
    b.add("representations", Value(std::move(reps)));
    return b.take();
}

}  // namespace

std::string serializeManifest(const Manifest& m) {
    Value::Array playlists;
    for (const auto& p : m.playlists) playlists.push_back(toJson(p));
    ObjectBuilder b;
    b.add("format", Value(toString(m.format)))
        .add("source", Value(m.sourcePath.filename().string()))
        .add("base_url", Value(m.baseUrl))
        .add("playlists", Value(std::move(playlists)));
    return json::dump(b.take(), 2);
}

}  // namespace mediacore
