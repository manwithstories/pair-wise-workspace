#include "mediacore/manifest.hpp"

#include <algorithm>
#include <cctype>
#include <fstream>
#include <sstream>

namespace mediacore {

const char* toString(ManifestFormat fmt) {
    switch (fmt) {
        case ManifestFormat::Hls: return "hls";
        case ManifestFormat::Dash: return "dash";
        case ManifestFormat::Unknown: return "unknown";
    }
    return "unknown";
}

ManifestFormat detectFormat(const std::filesystem::path& p) {
    std::string ext = p.extension().string();
    std::transform(ext.begin(), ext.end(), ext.begin(),
                   [](unsigned char c) { return std::tolower(c); });
    if (ext == ".m3u8" || ext == ".m3u") return ManifestFormat::Hls;
    if (ext == ".mpd") return ManifestFormat::Dash;
    return ManifestFormat::Unknown;
}

// Implemented in hls_parser.cpp / dash_parser.cpp.
Manifest parseHls(const std::filesystem::path& path,
                  const ParseOptions& opts, ErrorLog& errors);
Manifest parseDash(const std::filesystem::path& path,
                   const ParseOptions& opts, ErrorLog& errors);

Manifest parseManifest(const std::filesystem::path& path,
                       const ParseOptions& opts, ErrorLog& errors) {
    const ManifestFormat fmt = detectFormat(path);
    if (fmt == ManifestFormat::Unknown) {
        errors.add({ErrorCode::InvalidArgument, "", -1, path.string(),
                    {0, 0, {0, 1}}, false,
                    "unrecognized manifest extension"});
        Manifest m;
        m.sourcePath = path;
        return m;
    }
    std::error_code ec;
    if (!std::filesystem::is_regular_file(path, ec)) {
        errors.add({ErrorCode::ManifestInvalid, "", -1, path.string(),
                    {0, 0, {0, 1}}, false, "manifest file not found"});
        Manifest m;
        m.format = fmt;
        m.sourcePath = path;
        return m;
    }
    if (fmt == ManifestFormat::Hls) return parseHls(path, opts, errors);
    return parseDash(path, opts, errors);
}

std::vector<std::string> splitComma(const std::string& s) {
    std::vector<std::string> out;
    std::string token;
    std::stringstream ss(s);
    while (std::getline(ss, token, ',')) {
        if (!token.empty()) out.push_back(token);
    }
    return out;
}

json::Value toJson(const TrackSpec& spec) {
    json::ObjectBuilder b;
    b.add("id", json::Value(spec.id))
        .add("playlist", json::Value(spec.playlist))
        .add("role", json::Value(spec.role))
        .add("expected_media_type", json::Value(spec.expectedMediaType))
        .add("expected_channels", json::Value(spec.expectedChannels))
        .add("expected_sample_rate", json::Value(spec.expectedSampleRate));
    return b.take();
}

std::vector<TrackSpec> loadTracks(const std::filesystem::path& path,
                                  ErrorLog& errors) {
    std::vector<TrackSpec> out;
    std::ifstream in(path);
    if (!in) {
        errors.add({ErrorCode::InvalidArgument, "", -1, path.string(),
                    {0, 0, {0, 1}}, false, "cannot open tracks.json"});
        return out;
    }
    // The fixture generator owns this file; keep the parser dependency-free
    // by accepting a deliberately small line-oriented JSON subset.
    TrackSpec cur;
    bool have = false;
    std::string line;
    auto flush = [&] {
        if (have && !cur.id.empty()) out.push_back(std::move(cur));
        cur = TrackSpec{};
        have = false;
    };
    auto field = [](const std::string& l) {
        auto k = l.find(':');
        if (k == std::string::npos) return std::pair<std::string, std::string>{};
        auto key = l.substr(0, k);
        auto val = l.substr(k + 1);
        auto trim = [](std::string& s) {
            while (!s.empty() && (s.front() == ' ' || s.front() == '"'))
                s.erase(s.begin());
            while (!s.empty() &&
                   (s.back() == ' ' || s.back() == '"' || s.back() == ','))
                s.pop_back();
        };
        trim(key);
        trim(val);
        return std::pair{key, val};
    };
    while (std::getline(in, line)) {
        if (line.find('{') != std::string::npos) {
            flush();
            have = true;
            continue;
        }
        if (line.find('}') != std::string::npos) flush();
        auto [k, v] = field(line);
        if (!have || k.empty()) continue;
        if (k == "id") cur.id = v;
        else if (k == "playlist") cur.playlist = v;
        else if (k == "role") cur.role = v;
        else if (k == "expected_media_type") cur.expectedMediaType = v;
        else if (k == "expected_channels") cur.expectedChannels = std::stoi(v);
        else if (k == "expected_sample_rate")
            cur.expectedSampleRate = std::stoi(v);
    }
    flush();
    return out;
}

}  // namespace mediacore
