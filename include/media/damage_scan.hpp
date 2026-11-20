#pragma once
// Damage scanning: truncation, indexed page damage (CRC), packet spacing
// (keyframe gap), decode errors and index recovery.
#include <cstdint>
#include <string>
#include <vector>

#include "media/probe.hpp"
#include "media/timeline.hpp"

namespace mediaqc {

struct ScanConfig {
    int64_t keyframe_gap_ns = 250'000'000;  // 250 ms default
    int frames_per_page = 48000;            // 1 s index pages at 48 kHz
};

struct ScanResult {
    std::vector<DamageRecord> damage;       // unified error model
    int64_t first_damage_offset = -1;       // earliest source byte with damage
    int64_t truncated_bytes = 0;            // bytes dropped at tail truncation
    bool index_was_rebuilt = false;
    bool unrecoverable = false;
    // Affected timeline range (union of all damage ranges).
    int64_t affected_start_sample = 0;
    int64_t affected_end_sample = 0;
    int64_t packet_count = 0;
    int64_t total_frames = 0;
};

// Full scan of one probed input. Index pages are validated against current
// content; a missing sidecar triggers sequential rebuild from the last
// decodable boundary.
ScanResult scan_damage(ProbeResult& probe, const ScanConfig& cfg);

// Pure packet-spacing check, exposed for unit tests. Reports keyframe gaps and
// inconsistent packet durations; keyframes are every packet for PCM.
struct GapReport {
    std::vector<DamageRecord> damage;
};
GapReport check_packet_gaps(const std::vector<PacketInfo>& packets,
                            int sample_rate, int64_t gap_threshold_ns);

}  // namespace mediaqc
