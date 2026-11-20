#pragma once
// Per-track processing pipeline: probe -> index recovery -> damage scan ->
// (checkpoint reuse or repack) -> level metering -> manifest entry.
#include <optional>
#include <string>

#include "media/checkpoint.hpp"
#include "media/damage_scan.hpp"
#include "media/fs_util.hpp"
#include "media/manifest.hpp"
#include "media/repack.hpp"
#include "media/track_graph.hpp"

namespace mediaqc {

struct PipelineOptions {
    int target_rate_hz = 48000;
    int target_channels = 6;
    int64_t target_bitrate = 4'608'000;
    int64_t keyframe_gap_ms = 250;
    DamagePolicy damage_policy = DamagePolicy::Silence;
    bool resume = false;
};

// Full processing of one block. nullopt means an unrecoverable input; the
// damage details are logged to stderr before returning.
std::optional<TrackManifest> process_track(const InputBlock& block,
                                           const PipelineOptions& opts,
                                           const std::string& validated_out_dir,
                                           CheckpointStore& checkpoint,
                                           const std::string& param_digest,
                                           const ::mediaqc::fs::StatInfo& source_stat);

// Deterministic digest of the processing parameters for checkpoint validation.
std::string parameter_digest(const PipelineOptions& opts);

}  // namespace mediaqc
