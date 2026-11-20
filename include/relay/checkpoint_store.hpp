#pragma once

// Checkpoint store: atomically persists the pipeline cursor, running
// counters and completed damage ranges. A checkpoint binds the input
// content hash and the analysis version; loading validates both so a
// changed input or stale format refuses resume instead of mixing results.

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

#include "relay/error_model.hpp"

namespace relay {

struct Checkpoint {
  std::string input_sha256;      // hash of all analysis inputs
  uint32_t analysis_version = 0;
  uint64_t cursor = 0;           // packet records fully processed
  uint64_t duplicate_packets = 0;
  uint64_t reordered_packets = 0;
  uint64_t lost_packets = 0;
  uint64_t burst_losses = 0;
  std::vector<DamageRange> completed_ranges;
};

// Write-temp-fsync-rename; a crash mid-save never leaves a torn checkpoint.
bool save_checkpoint(const std::string& path, const Checkpoint& ckpt);

enum class LoadStatus { Ok, NotFound, Corrupt, InputMismatch, VersionMismatch };
struct LoadResult {
  LoadStatus status = LoadStatus::NotFound;
  Checkpoint checkpoint;
  std::string detail;
};

// expected_input_hash / expected_version are validated against the file.
LoadResult load_checkpoint(const std::string& path,
                           const std::string& expected_input_hash,
                           uint32_t expected_version);

}  // namespace relay
