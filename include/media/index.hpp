#pragma once

#include <array>
#include <cstdint>
#include <string>
#include <vector>

#include "media/json.hpp"
#include "media/types.hpp"

namespace mediaqc {

// The repaired output is a directory of self-contained MP4 segments, one
// stream each.  Segments make resume honest: an interrupted run leaves whole
// segments behind, each independently digestible and seekable, instead of a
// half-written container that cannot be appended to.
//
//   <output>/index.json
//   <output>/manifest.json
//   <output>/media/<track>.seg00000.mp4 ...
struct SegmentInfo {
  std::string track;
  std::string path;          // relative to the output directory
  std::uint64_t first_frame = 0;  // first source frame index in this segment
  std::uint64_t frames = 0;
  Ns start_ns = 0;
  Ns end_ns = 0;             // exclusive
  std::string sha256;        // digest of the segment file
  std::uint64_t bytes = 0;
};

// One random-access point, addressable by time or by source frame number.
struct IndexEntry {
  Ns time_ns = 0;
  std::uint64_t frame = 0;      // source sequence number
  std::uint64_t frame_index = 0;  // 0-based ordinal within the track
  std::string track;
  std::size_t segment = 0;      // index into MediaIndex::segments
  std::uint64_t payload_offset = 0;  // byte offset inside the segment file
  std::uint64_t payload_size = 0;
  bool keyframe = false;
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
  std::uint64_t frames = 0;
  std::size_t segments = 0;
  std::size_t entries = 0;
};

struct MediaIndex {
  std::uint64_t frames_total = 0;
  std::uint64_t frames_repaired = 0;
  std::uint64_t frames_dropped = 0;
  Ns duration_ns = 0;
  std::string output_sha256;   // digest of index.json's own content minus this
  std::string input_sha256;
  std::vector<IndexTrack> tracks;
  std::vector<SegmentInfo> segments;
  std::vector<IndexEntry> entries;

  [[nodiscard]] Json to_json() const;

  // Writes `<dir>/index.json` atomically.
  [[nodiscard]] bool write(const std::string& dir, Error& error) const;
  [[nodiscard]] static std::string path_in(const std::string& dir);
};

// Loads an index; validates structure and that every referenced segment stays
// inside the output directory.
[[nodiscard]] bool load_index(const std::string& dir, MediaIndex& out,
                              Error& error);

// Binary searches over time/frame within one track (entries per track are
// sorted).  Returns nullptr when the track has no entries.
[[nodiscard]] const IndexEntry* find_by_time(const MediaIndex& index,
                                             const std::string& track,
                                             Ns time_ns);
[[nodiscard]] const IndexEntry* find_by_frame(const MediaIndex& index,
                                              const std::string& track,
                                              std::uint64_t frame);

}  // namespace mediaqc
