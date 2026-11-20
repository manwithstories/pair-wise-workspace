#pragma once

#include <cstdint>
#include <filesystem>
#include <optional>
#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/json.hpp"
#include "mediacore/time.hpp"

namespace mediacore {

enum class ManifestFormat { Hls, Dash, Unknown };
const char* toString(ManifestFormat fmt);
ManifestFormat detectFormat(const std::filesystem::path& manifestPath);

// One media segment (HLS .ts/.m4s entry or DASH SegmentTemplate chunk).
struct SegmentRef {
    int64_t sequence = 0;          // media sequence / explicit index
    int64_t discontinuity = 0;    // HLS EXT-X-DISCONTINUITY counter
    Rational duration{0, 1};      // declared duration in seconds
    std::optional<Tick> startTick;  // explicit DASH t / HLS program time
    Rational tickBase{1, 1};
    std::string uri;
    std::string initUri;          // DASH initialization / HLS map
    bool byterange = false;
    bool independent = false;     // EXT-X-INDEPENDENT-SEGMENTS flag
};

// DASH Representation or a single-variant HLS playlist.
struct Representation {
    std::string id;
    std::string contentType;       // video / audio / subtitle
    int64_t bandwidthBps = 0;
    std::vector<std::string> codecs;
    std::string lang;
    Rational frameRate{0, 1};
    int width = 0;
    int height = 0;
    std::vector<SegmentRef> segments;
};

struct MediaPlaylist {
    std::string trackId;          // logical track, from tracks.json
    std::string name;             // playlist file / Representation id
    std::string uri;              // playlist path relative to manifest dir
    std::string mediaType;        // video / audio / mixed / subtitle
    Rational targetDuration{0, 1};
    bool ended = false;           // EXT-X-ENDLIST / full MPD timeline
    bool independentSegments = false;
    std::optional<Tick> programStart;  // EXT-X-PROGRAM-DATE-TIME origin
    std::vector<Representation> representations;
};

struct Manifest {
    ManifestFormat format = ManifestFormat::Unknown;
    std::filesystem::path sourcePath;
    std::string baseUrl;
    std::vector<MediaPlaylist> playlists;
};

struct FieldDiff {
    std::string track;
    std::string field;
    std::string expected;
    std::string actual;
};

struct RoundTripReport {
    std::size_t comparedFields = 0;
    std::size_t matchedFields = 0;
    std::vector<FieldDiff> mismatches;
    double consistency() const {
        return comparedFields == 0
                   ? 1.0
                   : static_cast<double>(matchedFields) / comparedFields;
    }
};

struct ParseOptions {
    std::filesystem::path root;       // containment root
    bool strict = false;
};

// Dispatch on file extension. Fatal errors (truncation, invalid syntax) go
// into the log with recoverable=false and the returned manifest may hold
// whatever partial content was parsed.
Manifest parseManifest(const std::filesystem::path& path,
                       const ParseOptions& opts, ErrorLog& errors);

// Canonical normalized serialization of everything we retain.
std::string serializeManifest(const Manifest& m);

// Parse -> serialize -> parse and compare every retained field.
RoundTripReport roundTripCompare(const Manifest& m);

// Rebuild a Manifest from canonical JSON emitted by serializeManifest.
Manifest materializeManifest(const std::string& canonicalJson);

// tracks.json: maps logical track ids to playlist files and expected specs.
struct TrackSpec {
    std::string id;
    std::string playlist;
    std::string role;               // interview / bcam / narration ...
    std::string expectedMediaType;  // video / audio / av
    int expectedChannels = 0;       // 0 == unknown / not constrained
    int expectedSampleRate = 0;
};

std::vector<TrackSpec> loadTracks(const std::filesystem::path& path,
                                  ErrorLog& errors);
json::Value toJson(const TrackSpec& spec);

}  // namespace mediacore
