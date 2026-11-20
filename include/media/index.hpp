#pragma once

#include <array>
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

#include "media/json.hpp"
#include "media/types.hpp"

namespace mediaqc {

// One random-access point: a remark that lets a player seek by time or by
// source frame number.  `time_ns` is on the unified timeline, `frame` is the
// source sequence number, and `payload_offset`/`payload_size` locate the
// packet inside the output file so lookups do not require a second demux.
struct IndexEntry {
  Ns time_ns = 0;
  std::uint64_t frame = 0;         // source sequence number
  std::uint64_t output_packet = 0; // ordinal in write order (0-based per track)
  std::uint64_t payload_offset = 0;  // byte offset inside the output file
  std::uint64_t payload_size = 0;
  bool keyframe = false;
  std::string track;
  std::array<std::uint8_t, 32> sha256{};  // source frame checksum
};

struct IndexTrack {
  std::string id;
  TrackKind kind = TrackKind::Video;
  std::string codec;
  int width = 0;
  int height = 0;
  int sample_rate = 0;
  int channels = 0;
  Rational rate;
  std::size_t entries = 0;
};

// The sidecar index written next to the output container.  It is a
// self-describing JSON document so archival tools can consume it without
// linking FFmpeg.
struct MediaIndex {
  std::uint64_t frames_total = 0;
  std::uint64_t frames_repaired = 0;
  std::uint64_t frames_dropped = 0;
  Ns duration_ns = 0;
  std::string output_sha256;
  std::string input_sha256;
  std::vector<IndexTrack> tracks;
  std::vector<IndexEntry> entries;

  // Writes `<directory>/<stem>.index.json`.  Returns the path written.
  [[nodiscard]] bool write(const std::string& output_path, Error& error) const;
  [[nodiscard]] std::string sidecar_path(const std::string& output_path) const;
  [[nodiscard]] Json to_json() const;
};

// Loads a sidecar index for verification.
[[nodiscard]] bool load_index(const std::string& index_path, MediaIndex& out,
                              Error& error);

// Locates the entry nearest to `time_ns` (binary search; entries are sorted).
[[nodiscard]] const IndexEntry* find_by_time(const MediaIndex& index,
                                             Ns time_ns);
// Locates the entry with the greatest frame <= `frame`.
[[nodiscard]] const IndexEntry* find_by_frame(const MediaIndex& index,
                                              std::uint64_t frame);

}  // namespace mediaqc
