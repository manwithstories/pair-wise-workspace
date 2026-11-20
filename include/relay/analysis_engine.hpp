#pragma once

// Analysis engine: chains transport reorder, stream probe and caption
// parsing onto the unified time axis, attributes damage by origin, merges
// overlapping ranges per kind, and supports checkpointed interrupt/resume.
// Resumed runs re-verify the input hash and analysis version and must
// produce byte-identical output checksums.

#include <cstdint>
#include <string>
#include <vector>

#include "relay/error_model.hpp"

namespace relay {

struct EngineInputs {
  std::string packets_path;    // CSV: node,stream,seq,ts_ms,arrival_ms,bytes
  std::string media_path;      // optional; empty skips probing
  std::string captions_path;   // optional; empty skips caption parsing
  std::string expected_damage_path;  // optional; injected-truth sidecar
};

struct EngineConfig {
  size_t packet_window = 64;
  int64_t memory_limit_bytes = 32ll << 20;
  uint64_t interrupt_after_chunks = 0;  // 0 = run to completion
  bool resume = false;
  std::string checkpoint_path;
  uint32_t analysis_version = 1;
};

enum class RunStatus {
  Ok,
  Interrupted,         // checkpoint saved; safe to resume
  CheckpointMismatch,  // input or version changed; resume refused
  CheckpointCorrupt,
  InputError,
};

struct EngineResult {
  RunStatus status = RunStatus::Ok;
  std::string detail;
  std::vector<DamageRange> damaged_ranges;
  uint64_t duplicate_packets = 0;
  uint64_t reordered_packets = 0;
  uint64_t lost_packets = 0;
  uint64_t burst_losses = 0;
  int64_t peak_memory_bytes = 0;
  double est_bandwidth_kbps = 0.0;
  double resume_ms = 0.0;        // wall time of a resumed run
  int64_t boundary_error_ms = -1;  // -1 when no expected-damage sidecar
  std::string output_sha256;
};

EngineResult run_analysis(const EngineInputs& inputs, const EngineConfig& cfg);

// Merge overlapping/adjacent ranges of the same kind on the unified axis.
std::vector<DamageRange> merge_damage_ranges(std::vector<DamageRange> in,
                                             int64_t tolerance_ms);

}  // namespace relay
