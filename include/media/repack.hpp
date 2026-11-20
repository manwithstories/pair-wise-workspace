#pragma once
// Re-packing: ordered by the track graph, PCM passthrough within tolerance or
// libswresample conversion; damaged ranges filled with silence or cut.
#include <cstdint>
#include <string>
#include <vector>

#include "media/damage_scan.hpp"
#include "media/probe.hpp"
#include "media/timeline.hpp"

namespace mediaqc {

enum class DamagePolicy { Silence, Truncate };

struct RepackConfig {
    int target_rate_hz = 48000;
    int target_channels = 6;
    int64_t target_bitrate = 0;       // bits per second, 0 = derive from source
    int64_t passthrough_tolerance_bps = 0;  // allow PCM reuse within this delta
    DamagePolicy damage_policy = DamagePolicy::Silence;
};

struct RepackResult {
    std::string output_path;
    int64_t output_bytes = 0;
    int64_t output_frames = 0;
    int output_rate_hz = 0;
    int output_channels = 0;
    int bits_per_sample = 16;
    int64_t actual_bitrate_bps = 0;
    double bitrate_deviation_pct = 0;
    bool pcm_reused = false;
    int64_t silence_frames = 0;       // frames filled due to damage policy
    int64_t dropped_frames = 0;       // frames cut due to truncate policy
    std::vector<Segment> policy_ranges;  // boundaries the policy was applied to
};

// Re-pack one probed+scanned source into a WAV inside validated_out_dir.
// Writes via a temp file and atomic replacement; never touches the source.
RepackResult repack(const ProbeResult& probe, const ScanResult& scan,
                    const RepackConfig& cfg, const std::string& validated_out_dir,
                    const std::string& output_name);

}  // namespace mediaqc
