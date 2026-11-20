#pragma once

#include <filesystem>
#include <map>
#include <string>
#include <vector>

#include "relay/json.hpp"
#include "relay/ladder.hpp"
#include "relay/segmenter.hpp"

namespace relay {

struct ManifestSegment {
  TrackId track;
  std::uint64_t index = 0;
  Micros start_us = 0;
  Micros end_us = 0;
  std::uint32_t start_sequence = 0;
  std::uint32_t end_sequence = 0;
  std::size_t frame_count = 0;
  std::string status;
  std::string status_reason;
  std::string content_hash;
  std::string rendition;
  std::uint64_t bitrate_bps = 0;
  bool random_access = false;
  /// Arbitrary producer extensions that must survive a round-trip.
  json::Object extensions;
};

struct ManifestTrack {
  std::string id;
  std::string state;  // "ready" / "blocked" / "empty"
  std::string dependency_chain;
  int epochs = 1;
  std::size_t keyframes = 0;
  std::size_t losses = 0;
  std::vector<ManifestSegment> segments;
  json::Object extensions;
};

struct Manifest {
  int version = 1;
  std::string input_digest;
  std::string config_digest;
  Timebase timebase;
  Micros start_us = 0;
  Micros end_us = 0;
  std::vector<ManifestTrack> tracks;
  std::vector<DowngradeEvent> downgrades;
  /// Top-level fields this consumer does not understand.  They are written
  /// back unchanged, so a newer producer/older consumer pair stays lossless.
  json::Object unknown_fields;

  [[nodiscard]] std::size_t segments_count() const;
};

struct ManifestParseResult {
  bool ok = false;
  std::string error;
  std::size_t error_offset = 0;
  /// Set when the manifest was structurally valid but a record was
  /// rejected; isolated records are listed here and the rest is usable.
  std::vector<std::string> isolated;
};

/// Serialises the manifest as deterministic, key-sorted JSON.
std::string serialize(const Manifest& manifest);

/// Parses a manifest, preserving unknown fields.  Individual malformed
/// segment records are isolated in `result.isolated`; a damaged document
/// itself yields ok=false so callers never act on an unverified manifest.
ManifestParseResult parse_manifest(const std::string& text, Manifest& out);

/// Reads/writes through the filesystem; writes go to a temporary sibling
/// and are atomically renamed so a partial manifest is never observed.
ManifestParseResult load_manifest(const std::filesystem::path& path,
                                  Manifest& out);
bool store_manifest(const std::filesystem::path& path, const Manifest& manifest,
                    std::string& error);

/// SHA-256 of the serialized manifest; used by the stage cache to detect a
/// change in what a downstream stage consumed.
std::string manifest_digest(const Manifest& manifest);

}  // namespace relay
