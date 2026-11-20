#include <string>
#include <vector>

#include "mediacore/json.hpp"
#include "mediacore/manifest.hpp"

namespace mediacore {

namespace {

int64_t asInt(const json::Value::Object& o, const std::string& key,
              int64_t fallback = 0) {
    auto it = o.find(key);
    if (it == o.end()) return fallback;
    // Stored numbers are int64_t in our variant; visit defensively.
    int64_t out = fallback;
    visitValue(it->second, [&](const auto& x) {
        using T = std::decay_t<decltype(x)>;
        if constexpr (std::is_same_v<T, int64_t>) out = x;
    });
    return out;
}

bool asBool(const json::Value::Object& o, const std::string& key) {
    auto it = o.find(key);
    if (it == o.end()) return false;
    bool out = false;
    visitValue(it->second, [&](const auto& x) {
        using T = std::decay_t<decltype(x)>;
        if constexpr (std::is_same_v<T, bool>) out = x;
    });
    return out;
}

std::string asStr(const json::Value::Object& o,
                  const std::string& key) {
    auto it = o.find(key);
    if (it == o.end()) return {};
    std::string out;
    visitValue(it->second, [&](const auto& x) {
        using T = std::decay_t<decltype(x)>;
        if constexpr (std::is_same_v<T, std::string>) out = x;
    });
    return out;
}

Rational asRational(const json::Value::Object& o,
                    const std::string& key) {
    auto it = o.find(key);
    if (it == o.end()) return Rational{0, 1};
    const auto& sub = it->second.asObject();
    return Rational{asInt(sub, "num"), asInt(sub, "den", 1)};
}

SegmentRef materializeSegment(const json::Value::Object& o) {
    SegmentRef s;
    s.sequence = asInt(o, "sequence");
    s.discontinuity = asInt(o, "discontinuity");
    s.duration = asRational(o, "duration");
    s.uri = asStr(o, "uri");
    s.initUri = asStr(o, "init_uri");
    s.byterange = asBool(o, "byterange");
    s.independent = asBool(o, "independent");
    auto tick = o.find("start_tick");
    if (tick != o.end()) {
        int64_t t = 0;
        visitValue(tick->second, [&](const auto& x) {
            using T = std::decay_t<decltype(x)>;
            if constexpr (std::is_same_v<T, int64_t>) t = x;
        });
        s.startTick = t;
        s.tickBase = asRational(o, "tick_base");
    }
    return s;
}

Representation materializeRep(const json::Value::Object& o) {
    Representation r;
    r.id = asStr(o, "id");
    r.contentType = asStr(o, "content_type");
    r.bandwidthBps = asInt(o, "bandwidth_bps");
    r.lang = asStr(o, "lang");
    r.frameRate = asRational(o, "frame_rate");
    r.width = static_cast<int>(asInt(o, "width"));
    r.height = static_cast<int>(asInt(o, "height"));
    auto codecs = o.find("codecs");
    if (codecs != o.end())
        for (const auto& c : codecs->second.asArray()) {
            std::string s;
            visitValue(c, [&](const auto& x) {
                using T = std::decay_t<decltype(x)>;
                if constexpr (std::is_same_v<T, std::string>) s = x;
            });
            r.codecs.push_back(s);
        }
    auto segs = o.find("segments");
    if (segs != o.end())
        for (const auto& v : segs->second.asArray())
            r.segments.push_back(materializeSegment(v.asObject()));
    return r;
}

MediaPlaylist materializePlaylist(const json::Value::Object& o) {
    MediaPlaylist p;
    p.trackId = asStr(o, "track_id");
    p.name = asStr(o, "name");
    p.uri = asStr(o, "uri");
    p.mediaType = asStr(o, "media_type");
    p.targetDuration = asRational(o, "target_duration");
    p.ended = asBool(o, "ended");
    p.independentSegments = asBool(o, "independent_segments");
    auto start = o.find("program_start");
    if (start != o.end()) {
        int64_t t = 0;
        visitValue(start->second, [&](const auto& x) {
            using T = std::decay_t<decltype(x)>;
            if constexpr (std::is_same_v<T, int64_t>) t = x;
        });
        p.programStart = t;
    }
    auto reps = o.find("representations");
    if (reps != o.end())
        for (const auto& v : reps->second.asArray())
            p.representations.push_back(materializeRep(v.asObject()));
    return p;
}

}  // namespace

Manifest materializeManifest(const std::string& canonicalJson) {
    Manifest m;
    json::Value root;
    if (!json::parse(canonicalJson, root)) return m;
    const auto& top = root.asObject();
    const std::string fmt = asStr(top, "format");
    m.format = fmt == "dash" ? ManifestFormat::Dash : ManifestFormat::Hls;
    m.baseUrl = asStr(top, "base_url");
    m.sourcePath = asStr(top, "source");
    auto it = top.find("playlists");
    if (it != top.end())
        for (const auto& v : it->second.asArray())
            m.playlists.push_back(materializePlaylist(v.asObject()));
    return m;
}

}  // namespace mediacore
