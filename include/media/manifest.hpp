#pragma once
// Machine-readable quality manifest aligned with the unified timeline.
#include <cstdint>
#include <string>
#include <vector>

#include "media/damage_scan.hpp"
#include "media/json.hpp"
#include "media/probe.hpp"
#include "media/repack.hpp"
#include "media/timeline.hpp"

namespace mediaqc {

struct LevelStats {
    double peak_dbfs = -144.0;
    double rms_dbfs = -144.0;
};

// Streaming s16 PCM level meter over one output WAV (bounded memory).
LevelStats measure_levels(const std::string& wav_path, int channels);

struct TrackManifest {
    std::string track_id;
    std::string source_path;
    std::string output_path;
    int64_t start_ns = 0;
    int64_t end_ns = 0;
    int64_t packets = 0;
    int64_t damaged_ranges = 0;
    int64_t truncated_bytes = 0;
    int64_t target_bitrate = 0;
    int64_t actual_bitrate = 0;
    double bitrate_deviation_pct = 0;
    double peak_dbfs = 0;
    double rms_dbfs = 0;
    int64_t alignment_error_ns = 0;  // output vs timeline mapping residual
    bool pcm_reused = false;
    std::string damage_policy;
    std::vector<DamageRecord> damage;
    std::vector<Segment> policy_ranges;
    bool index_rebuilt = false;
};

struct BatchManifest {
    int output_rate_hz = 48000;
    int output_channels = 6;
    int64_t target_bitrate = 0;
    int64_t max_alignment_error_ns = 0;
    bool has_recoverable_damage = false;
    bool unrecoverable = false;
    std::vector<TrackManifest> tracks;
};

// Serialize/parse the batch manifest (also consumed by `verify`).
std::string render_manifest(const BatchManifest& m);
BatchManifest parse_manifest(const std::string& path);

}  // namespace mediaqc
