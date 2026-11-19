#pragma once

#include <cstdint>
#include <filesystem>
#include <optional>
#include <string>
#include <vector>

#include "error.hpp"
#include "file_io.hpp"
#include "rational.hpp"

namespace media_restore {

enum class Codec { h264, h265, av1 };
enum class TrackKind { video, audio };

const char* codec_name(Codec codec);
const char* track_kind_name(TrackKind kind);
bool parse_codec(std::string_view text, Codec* out);
bool parse_track_kind(std::string_view text, TrackKind* out);

// One entry of a versioned keyframe index.  `tick` is media time in the
// manifest time base, `offset` a byte position in the raw bitstream.
struct IndexEntry {
  int64_t tick = 0;
  int64_t offset = 0;
  bool keyframe = true;
};

struct KeyframeIndex {
  bool present = false;
  std::string path;
  int version = 0;
  std::vector<IndexEntry> entries;
};

// Accepted index format versions.  Version 1 is the flat entry list; version
// 2 additionally carries a `codec` field that must match the segment.
constexpr int kIndexVersionMin = 1;
constexpr int kIndexVersionMax = 2;

struct ColorMetadata {
  std::optional<std::string> primaries;
  std::optional<std::string> transfer;
  std::optional<std::string> matrix;
  std::optional<std::string> range;
  std::string source;  // where the values were recorded, e.g. "reel_a"
};

struct AudioInfo {
  int sample_rate = 0;
  std::string layout;
  Rational offset;   // relative to the segment start; may be negative
  std::string media; // archive-relative audio file; empty means segment.media
};

struct Segment {
  std::string id;
  Rational start;
  Rational duration;
  std::string media;  // archive-relative path
  Codec codec = Codec::h264;
  KeyframeIndex index;
  ColorMetadata color;
  std::optional<AudioInfo> audio;
  // Damaged material is declared in the manifest so its rejection is an
  // expected, reproducible outcome rather than an unhandled failure.
  std::optional<ErrorCode> expected_code;
};

struct Track {
  std::string id;
  TrackKind kind = TrackKind::video;
  std::vector<std::string> depends_on;
  std::vector<Segment> segments;
};

struct Manifest {
  std::string format = "media-archive-manifest";
  int version = 1;
  Rational time_base{1, 90000};
  std::vector<Track> tracks;
  std::filesystem::path source_path;

  const Track* find_track(std::string_view id) const;
  const Segment* find_segment(std::string_view track_id,
                              std::string_view segment_id) const;
  Rational segment_end(const Segment& segment) const;
};

constexpr int kManifestVersion = 1;

// Parses and schema-validates a manifest.  Diagnostics carry track, segment
// and field locations so the CLI can point at the offending value.
bool parse_manifest(std::string_view text, Manifest* out,
                    std::vector<Diagnostic>* diagnostics);

bool load_manifest(const std::filesystem::path& path, const Limits& limits,
                   Manifest* out, std::vector<Diagnostic>* diagnostics);

// Canonical serialization: key order is fixed and times are written as exact
// "num/den" strings, so a saved manifest round-trips byte-for-byte.
std::string serialize_manifest(const Manifest& manifest);

}  // namespace media_restore
