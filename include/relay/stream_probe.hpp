#pragma once

// Stream probe: walks container packets with FFmpeg, reporting timestamps,
// keyframes and keyframe interval on the unified ms axis. Timestamp wrap,
// jumps and missing timestamps are attributed separately; sections that
// cannot be judged reliably are marked uncertain. An optional decode pass
// localizes structural corruption to the packet's presentation time.

#include <cstdint>
#include <string>
#include <vector>

#include "relay/error_model.hpp"
#include "relay/media_time.hpp"

namespace relay {

struct ProbeOptions {
  bool decode_check = true;  // decode video packets to surface corruption
};

struct ProbeStats {
  uint64_t packets = 0;
  uint64_t keyframes = 0;
  Rational frame_rate{0, 1};        // best-effort average frame rate
  Rational keyframe_interval_ms{0, 1};
  int64_t duration_ms = 0;
};

struct ProbeOutput {
  ProbeStats stats;
  std::vector<DamageRange> damages;  // unified-axis ranges, unattributed merged later
  bool ok = false;
  std::string error;                 // set when ok == false
};

ProbeOutput probe_media(const std::string& path, const ProbeOptions& opts);

// Locate the video keyframe packet nearest to `target_ms`; used by the
// sample-damaging tool to pick a precise injection point.
struct PacketLocation {
  int64_t pts_ms = 0;
  int64_t file_offset = 0;
  int64_t size = 0;
  bool found = false;
};
PacketLocation find_keyframe_near(const std::string& path, int64_t target_ms);

}  // namespace relay
