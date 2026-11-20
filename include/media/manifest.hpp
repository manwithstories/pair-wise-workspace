#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/index.hpp"
#include "media/json.hpp"
#include "media/types.hpp"

namespace mediaqc {

// The manifest travels with an output directory and lets an archivist prove
// what happened to every asset without re-running the pipeline: input and
// output digests, the repair journal, the damage list, the index digest and
// the resume cost.
struct ManifestTrack {
  std::string id;
  TrackKind kind = TrackKind::Video;
  std::string codec;
  std::uint64_t frames = 0;
  std::uint64_t repaired = 0;
  std::uint64_t dropped = 0;
  Rational rate;
};

struct Manifest {
  int version = 1;
  std::uint64_t frames_total = 0;
  std::uint64_t frames_repaired = 0;
  std::uint64_t frames_verified = 0;
  std::uint64_t frames_corrupt = 0;
  std::uint64_t frames_duplicate = 0;
  std::uint64_t frames_truncated = 0;
  std::int64_t first_corrupt_offset = -1;
  std::uint64_t peak_queue_frames = 0;
  std::uint64_t index_entries = 0;
  std::int64_t resume_ms = 0;
  bool resumed = false;
  std::string input_sha256;
  std::string output_sha256;
  std::string index_sha256;
  Ns duration_ns = 0;
  std::vector<ManifestTrack> tracks;
  std::vector<RepairRecord> repairs;
  Json errors;

  [[nodiscard]] Json to_json() const;
  [[nodiscard]] static bool from_json(const Json& value, Manifest& out,
                                      Error& error);
};

// Writes `<dir>/manifest.json` atomically.
[[nodiscard]] bool write_manifest(const std::string& dir, const Manifest& manifest,
                                  Error& error);

[[nodiscard]] bool load_manifest(const std::string& path, Manifest& out,
                                 Error& error);

}  // namespace mediaqc
