#pragma once

#include <cstdint>
#include <string>
#include <string_view>

#include "media/types.hpp"

namespace mediaqc {

// Scenarios the built-in sample generator can produce.
enum class Scenario {
  kNormal,
  kBoundary,      // wraparound + duplicates + missing times + out-of-order
  kWraparound,
  kDuplicate,
  kBadChecksum,
  kOutOfOrder,
  kTruncatedTail,
  kSlowConsumer,
};

[[nodiscard]] bool parse_scenario(std::string_view text, Scenario& out);
[[nodiscard]] std::string_view to_string(Scenario scenario);
[[nodiscard]] std::vector<std::string> scenario_names();

struct GenerateOptions {
  Scenario scenario = Scenario::kBoundary;
  std::uint64_t frames = 10000;  // total across all tracks
  std::uint64_t seed = 13307;
};

struct GenerateResult {
  bool ok = false;
  std::uint64_t frames = 0;
  std::uint64_t tracks = 0;
  // Byte offset of the first damaged frame record, or -1 when the scenario
  // has no damage.  Tests and the demo compare the pipeline's report against
  // this ground truth.
  std::int64_t first_corrupt_offset = -1;
  Error error;
};

// Writes a complete delivery into a new directory: delivery.json, one JSONL
// metadata stream per track and a matching media container per track.  The
// directory must not already exist unless `allow_existing` is set; nothing is
// ever overwritten silently.
[[nodiscard]] GenerateResult generate_delivery(const std::string& out_dir,
                                               const GenerateOptions& options,
                                               bool allow_existing = false);

}  // namespace mediaqc
