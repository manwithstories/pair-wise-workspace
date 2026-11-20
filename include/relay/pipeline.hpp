#pragma once

#include <cstdint>
#include <filesystem>
#include <map>
#include <string>
#include <vector>

#include "relay/manifest.hpp"
#include "relay/types.hpp"

namespace relay {

struct LadderPreset {
  std::string name;
  int height;
  std::uint64_t bitrate_bps;
};

struct PipelineConfig {
  std::filesystem::path work_dir;
  unsigned seed = 42;
  int frames = 240;
  int gop = 60;
  int width = 96;
  int height = 54;
  std::vector<std::string> cameras{"cam-a", "cam-b", "cam-c"};
  Micros segment_ms = 2000;
  int loss_burst = 5;
  std::vector<std::uint64_t> bandwidth{1800, 700, 2400};  // kbps
  std::vector<std::string> ladder{"360p", "720p", "1080p"};
  bool verify = false;
  bool corrupt = false;
  bool lose_keyframe = false;
  bool empty_track = false;
  std::size_t empty_camera_index = static_cast<std::size_t>(-1);
  bool interrupt_after_segment = false;
  std::size_t interrupt_segment = 0;
  bool resume = false;
  std::size_t duplicate_stride = 17;
  std::size_t reorder_stride = 11;
  std::size_t cache_limit_bytes = kMaxTrackCacheBytes;

  /// Stable digest of every option that affects output; used to reject a
  /// stale checkpoint when the run is reconfigured.
  [[nodiscard]] std::string config_digest() const;
};

struct PipelineReport {
  std::string input_digest;
  std::string config_digest;
  std::string manifest_digest;
  std::size_t tracks = 0;
  std::size_t usable_tracks = 0;
  std::size_t segments = 0;
  std::size_t random_access_segments = 0;
  std::size_t unseekable_segments = 0;
  std::size_t duplicates = 0;
  std::size_t reordered = 0;
  std::size_t lost_burst = 0;
  std::size_t late_dropped = 0;
  std::size_t epochs = 0;
  std::size_t checksum_failures = 0;
  std::size_t quarantined_frames = 0;
  std::size_t downgrades = 0;
  std::size_t resumed_stages = 0;
  std::size_t recomputed_stages = 0;
  std::size_t aligned_cut_error_frames = 0;
  double keyframe_hit_rate = 0.0;
  double roundtrip_rate = 0.0;
  std::uint64_t peak_buffer_bytes = 0;
  std::vector<std::string> segment_hashes;
  std::vector<std::string> blocked_tracks;
  std::vector<std::string> empty_tracks;
  std::vector<std::string> stage_order;
  bool interrupted = false;
  bool resumed = false;
};

struct PipelineResult {
  PipelineReport report;
  Manifest manifest;
  int exit_code = 0;
  std::filesystem::path manifest_path;
  std::vector<RelayError> errors;
};

/// Runs the full pipeline: read → reorder → timeline → segment → ladder →
/// manifest, honouring stage checkpoints.  An interrupt stops after the
/// configured segment and leaves a resumable checkpoint.
PipelineResult run_pipeline(const PipelineConfig& config);

/// Generates fixtures for `config` then runs the pipeline.  Used by the
/// `demo` command so a fresh work directory needs no external input.
PipelineResult run_demo(const PipelineConfig& config);

/// Persists/restores the effective run configuration next to the
/// checkpoints, so `resume` reproduces the original run exactly.
bool save_run_config(const std::filesystem::path& work_dir,
                     const PipelineConfig& config, std::string& error);
bool load_run_config(const std::filesystem::path& work_dir,
                     PipelineConfig& config, std::string& error);

}  // namespace relay
