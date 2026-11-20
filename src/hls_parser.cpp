#include <algorithm>
#include <fstream>
#include <map>
#include <sstream>

#include "mediacore/manifest.hpp"
#include "mediacore/pathutil.hpp"

namespace mediacore {
namespace {

// Parse HLS tag attributes honoring quoted strings.
std::map<std::string, std::string> attrsOf(const std::string& line) {
    std::map<std::string, std::string> out;
    const std::size_t colon = line.find(':');
    if (colon == std::string::npos) return out;
    const std::string body = line.substr(colon + 1);
    enum { KEY, VAL, QVAL } mode = KEY;
    std::string key, val;
    auto flush = [&] {
        if (!key.empty()) {
            if (!val.empty() && val.front() == '"') val.erase(val.begin());
            if (!val.empty() && val.back() == '"') val.pop_back();
            out[key] = val;
        }
        key.clear();
        val.clear();
    };
    for (char c : body) {
        if (mode == KEY) {
            if (c == '=') mode = VAL;
            else if (c == ',') flush();
            else key.push_back(c);
        } else if (mode == VAL) {
            if (c == '"') { val.push_back(c); mode = QVAL; }
            else if (c == ',') { flush(); mode = KEY; }
            else val.push_back(c);
        } else {
            val.push_back(c);
            if (c == '"') mode = VAL;
        }
    }
    flush();
    return out;
}

bool isTag(const std::string& l, const char* tag) {
    return l.rfind(tag, 0) == 0;
}

}  // namespace

// Forward declaration: media-playlist parser implemented below.
Manifest parseHlsMedia(const std::filesystem::path& path,
                       const ParseOptions& opts, ErrorLog& errors);

// Master playlists reference child media playlists; parse each child and
// merge their content into one manifest.
static Manifest parseHlsMaster(const std::filesystem::path& path,
                               const ParseOptions& opts,
                               ErrorLog& errors) {
    Manifest m;
    m.format = ManifestFormat::Hls;
    m.sourcePath = path;
    m.baseUrl = path.parent_path().string();
    const auto dir = path.parent_path();

    std::ifstream in(path);
    std::string line;
    bool expectChild = false;
    while (std::getline(in, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.rfind("#EXT-X-STREAM-INF", 0) == 0) {
            expectChild = true;
            continue;
        }
        if (line.rfind("#EXT-X-MEDIA:", 0) == 0) {
            // External audio rendition: capture its URI attribute.
            auto attrs = attrsOf(line);
            if (auto it = attrs.find("URI"); it != attrs.end()) {
                auto resolved =
                    resolveManifestUri(dir, it->second, opts.root);
                if (resolved) {
                    auto child =
                        parseHlsMedia(*resolved, opts, errors);
                    for (auto& pl : child.playlists)
                        m.playlists.push_back(std::move(pl));
                } else {
                    errors.add({ErrorCode::PathEscape, "", -1,
                                it->second, {0, 0, {1, 1}}, false,
                                "child playlist escapes input directory"});
                }
            }
            continue;
        }
        if (line.empty() || line.starts_with('#')) continue;
        if (expectChild) {
            auto resolved = resolveManifestUri(dir, line, opts.root);
            if (!resolved) {
                errors.add({ErrorCode::PathEscape, "", -1, line,
                            {0, 0, {1, 1}}, false,
                            "child playlist escapes input directory"});
            } else {
                auto child = parseHlsMedia(*resolved, opts, errors);
                for (auto& pl : child.playlists)
                    m.playlists.push_back(std::move(pl));
            }
            expectChild = false;
        }
    }
    return m;
}

Manifest parseHls(const std::filesystem::path& path,
                  const ParseOptions& opts, ErrorLog& errors) {
    // Detect master vs media playlist from the tag set before parsing.
    std::ifstream probeFile(path);
    std::string scan;
    bool isMaster = false;
    while (std::getline(probeFile, scan)) {
        // Exact tag names: "#EXT-X-MEDIA-SEQUENCE" must NOT match the
        // master-only "#EXT-X-MEDIA" rendition tag.
        const bool streamInf =
            scan.rfind("#EXT-X-STREAM-INF:", 0) == 0;
        const bool mediaRendition =
            scan.rfind("#EXT-X-MEDIA:", 0) == 0;
        if (streamInf || mediaRendition) {
            isMaster = true;
            break;
        }
    }
    if (isMaster) return parseHlsMaster(path, opts, errors);
    return parseHlsMedia(path, opts, errors);
}

Manifest parseHlsMedia(const std::filesystem::path& path,
                       const ParseOptions& opts, ErrorLog& errors) {
    Manifest m;
    m.format = ManifestFormat::Hls;
    m.sourcePath = path;
    m.baseUrl = path.parent_path().string();
    const auto dir = path.parent_path();

    std::ifstream in(path);
    std::string line;
    std::getline(in, line);
    if (!line.starts_with("#EXTM3U")) {
        errors.add({ErrorCode::ManifestInvalid, "", -1, path.string(),
                    {0, 0, {0, 1}}, false, "missing #EXTM3U header"});
        return m;
    }

    MediaPlaylist pl;
    pl.name = path.stem().string();
    pl.uri = path.filename().string();
    Representation rep;
    rep.id = pl.name;
    int64_t seq = 0;
    int64_t discontinuity = 0;
    std::string pendingMap;
    Rational pendingDuration{0, 1};
    bool haveDuration = false;
    bool anySegment = false;
    bool sawEnd = false;

    auto pushSegment = [&](const std::string& uri) {
        SegmentRef seg;
        seg.sequence = seq++;
        seg.discontinuity = discontinuity;
        seg.duration = pendingDuration;
        seg.uri = uri;
        seg.initUri = pendingMap;
        rep.segments.push_back(std::move(seg));
        haveDuration = false;
        anySegment = true;
    };

    while (std::getline(in, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.empty()) continue;
        if (isTag(line, "#EXT-X-VERSION")) continue;
        if (isTag(line, "#EXT-X-INDEPENDENT-SEGMENTS")) {
            pl.independentSegments = true;
            for (auto& s : rep.segments) s.independent = true;
            continue;
        }
        if (isTag(line, "#EXT-X-TARGETDURATION")) {
            auto v = parseDecimal(line.substr(line.find(':') + 1));
            if (v) pl.targetDuration = *v;
            continue;
        }
        if (isTag(line, "#EXT-X-MEDIA-SEQUENCE")) {
            seq = std::stoll(line.substr(line.find(':') + 1));
            continue;
        }
        if (isTag(line, "#EXT-X-DISCONTINUITY")) {
            ++discontinuity;
            continue;
        }
        if (isTag(line, "#EXT-X-MAP")) {
            auto a = attrsOf(line);
            if (auto it = a.find("URI"); it != a.end())
                pendingMap = it->second;
            continue;
        }
        if (isTag(line, "#EXT-X-ENDLIST")) {
            sawEnd = true;
            continue;
        }
        if (isTag(line, "#EXTINF")) {
            auto comma = line.find(',');
            std::string num = line.substr(line.find(':') + 1,
                comma == std::string::npos ? std::string::npos
                                           : comma - (line.find(':') + 1));
            auto v = parseDecimal(num);
            if (v) {
                pendingDuration = *v;
                haveDuration = true;
            }
            continue;
        }
        if (isTag(line, "#EXT-X-BYTERANGE")) {
            if (!rep.segments.empty()) rep.segments.back().byterange = true;
            continue;
        }
        if (line.starts_with('#')) continue;  // unsupported tags tolerated
        // Segment URI line.
        if (!haveDuration) {
            errors.add({ErrorCode::ManifestInvalid, pl.name,
                        static_cast<int>(seq), line,
                        {0, 0, {0, 1}}, false,
                        "segment without preceding EXTINF"});
        }
        auto resolved = resolveManifestUri(dir, line, opts.root);
        if (!resolved) {
            errors.add({ErrorCode::PathEscape, pl.name,
                        static_cast<int>(seq), line,
                        {0, 0, {0, 1}}, false,
                        "segment URI escapes input directory"});
        }
        pushSegment(line);
    }
    if (in.bad()) {
        errors.add({ErrorCode::ManifestTruncated, pl.name, -1,
                    path.string(), {0, 0, {0, 1}}, false,
                    "read failure while scanning playlist"});
    }
    if (anySegment && !sawEnd) {
        const auto& last = rep.segments.back();
        errors.add({ErrorCode::ManifestTruncated, pl.name,
                    static_cast<int>(last.sequence), pl.uri,
                    {0, 0, pendingDuration}, true,
                    "playlist missing EXT-X-ENDLIST (truncated)"});
    }
    pl.ended = sawEnd;
    if (rep.segments.empty() && !sawEnd)
        pl.ended = false;
    pl.representations.push_back(std::move(rep));
    m.playlists.push_back(std::move(pl));
    return m;
}

}  // namespace mediacore
