// End-to-end pipeline wiring the modules together:
// sample -> reader -> network replay -> jitter buffer -> recovery ->
// A/V sync -> checkpointed block index, producing the acceptance metrics.
#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <vector>

#include "media/checkpoint_index.hpp"
#include "media/network_replay.hpp"
#include "media/packet.hpp"

namespace media {

struct PipelineConfig {
  CodecType codec = CodecType::H264;
  std::uint32_t seed = 41;
  std::uint32_t frame_count = 120;
  std::uint32_t loss_burst = 0;
  std::vector<std::uint32_t> bandwidth_kbps;
  std::uint32_t inject_interrupt = 0; // interrupt after N blocks (0 = never)
  std::string checkpoint_dir;
  bool use_faulty_index = false;      // exercise damaged index fixtures
  bool truncated_stream = false;
};

struct PipelineMetrics {
  double sync_error_ms_p95 = 0;
  double recovery_rate = 1.0;
  double latency_p95_ms = 0;
  std::uint64_t packets_duplicate = 0;
  std::uint64_t packets_reordered = 0;
  std::uint64_t packets_dropped = 0;
  std::uint64_t checkpoint_resumed = 0;
  std::uint64_t blocks_written = 0;
  std::uint64_t total_packets = 0;
  std::map<std::string, std::uint64_t> errors_by_code;
  bool interrupted = false;
  bool checkpoint_corrupt = false;
};

// Full deterministic run. Returns 0 on success, 2 on corrupt input/index,
// 3 when a checkpoint cannot be recovered, matching the CLI exit contract.
int run_pipeline(const PipelineConfig& cfg, PipelineMetrics& metrics);

// Human-readable key=value report including every acceptance metric.
std::string format_metrics(const PipelineMetrics& m);

} // namespace media
