// timeline.hpp -- timestamp repair for a single track.
//
// The timeline consumes packets in demux order per stream and emits a
// monotonic DTS/PTS sequence expressed in the track's own time base.  It
// repairs missing timestamps, finite-range rollover and small backwards
// glitches, and records every repair.  Cases that cannot be repaired without
// guessing mark the track as failed instead of being silently dropped.
//
// Packets are streamed: the caller supplies a sink and the result only keeps
// bounded bookkeeping (statistics, repairs, resume state).  `retain_samples`
// exists for tests and small assets.
#pragma once

#include <cstdint>
#include <functional>
#include <string>
#include <vector>

#include "media_clock.hpp"

namespace mediaqc {

enum class RepairKind {
  None,
  DtsDerivedFromPts,
  PtsDerivedFromDts,
  WrapUnwrapped,
  BackwardsShifted,
  DuplicateShifted,
};

const char* repair_kind_name(RepairKind k);

struct TimestampRepair {
  RepairKind kind = RepairKind::None;
  int64_t input_dts = AV_NOPTS_VALUE;
  int64_t input_pts = AV_NOPTS_VALUE;
  int64_t output_dts = AV_NOPTS_VALUE;
  int64_t output_pts = AV_NOPTS_VALUE;
  int64_t adjustment = 0;  // ticks added by this repair
  int64_t byte_offset = -1;
  std::string reason;
};

struct TimelineSample {
  int64_t dts = AV_NOPTS_VALUE;  // repaired, stream time base
  int64_t pts = AV_NOPTS_VALUE;  // repaired, stream time base
  int64_t duration = 0;          // stream time base
  int64_t src_dts = AV_NOPTS_VALUE;
  int64_t src_pts = AV_NOPTS_VALUE;
  int64_t byte_offset = 0;
  int32_t byte_size = 0;
  int32_t flags = 0;
  int repair_index = -1;  // index into TimelineResult::repairs, or -1
};

struct TimelineStats {
  int64_t packets = 0;
  int64_t dts_missing = 0;
  int64_t pts_missing = 0;
  int64_t wrapped = 0;
  int64_t backwards_shifted = 0;
  int64_t duplicates = 0;
  int64_t unrepairable = 0;
  int64_t total_adjustment = 0;
  int64_t first_input_offset = -1;
};

// Everything needed to resume mid-track from a checkpoint.
struct TimelineState {
  int64_t last_out_dts = AV_NOPTS_VALUE;
  int64_t last_raw_dts = AV_NOPTS_VALUE;
  int64_t last_pts = AV_NOPTS_VALUE;
  int64_t last_duration = 0;
  int64_t wrap_offset = 0;
  int64_t shift = 0;
  int64_t packets = 0;
};

struct TimelineResult {
  Rational time_base;
  TimelineState state;
  TimelineStats stats;
  std::vector<TimestampRepair> repairs;
  std::vector<TimelineSample> samples;  // only when retain_samples is set
  bool ok = true;
  std::string failure;
  int64_t start_shift = 0;  // applied by timeline_align_starts
  int64_t first_out_dts = AV_NOPTS_VALUE;
  int64_t first_out_pts = AV_NOPTS_VALUE;
  int64_t gap_count = 0;  // discontinuities larger than the gap threshold
};

struct TimelineOptions {
  Rational time_base;
  WrapPolicy wrap;
  bool repair_missing = true;
  bool repair_backwards = true;
  bool reject_ambiguous = true;
  bool retain_samples = false;
  // Packets arriving with a jump larger than this are counted as gaps and
  // reported by the verifier; 0 -> period / 8 of the rollover policy.
  int64_t gap_threshold_ticks = 0;
  // Largest backwards jump that is treated as a glitch rather than an
  // ambiguous discontinuity.  Expressed in the stream time base; the default
  // is two seconds.
  int64_t ambiguity_tolerance_ticks = 0;  // 0 -> 2 * time_base
  // Upper bound on recorded repairs, to bound memory on damaged assets.
  int64_t max_repairs = 20000;
  int64_t estimated_duration = 0;  // fallback packet duration
};

using SampleSink = std::function<void(const TimelineSample&)>;

// Feeds one raw packet and invokes `sink` with the repaired sample.  Returns
// false once the timeline is unusable (`result.ok` false, reason in
// `result.failure`); the caller must then mark the asset failed.
bool timeline_push(TimelineOptions& opt, TimelineResult& result, int64_t src_dts,
                   int64_t src_pts, int64_t duration, int64_t byte_offset,
                   int32_t byte_size, int32_t flags, const SampleSink& sink);

// Aligns the first PTS of every track to a common origin by adding a per-track
// shift.  The minimum start across tracks becomes zero, so the relative
// audio/video start offset is preserved.  Tracks with no packets are ignored.
void timeline_align_starts(std::vector<TimelineResult*>& tracks);

// Maps a repaired stream timestamp into canonical 1/90000 ticks, including the
// track start shift.  Returns AV_NOPTS_VALUE for AV_NOPTS_VALUE.
int64_t timeline_to_canonical(const TimelineResult& track, int64_t ticks);

// First repaired PTS in canonical ticks (after alignment), or AV_NOPTS_VALUE.
int64_t timeline_first_canonical_pts(const TimelineResult& track);

}  // namespace mediaqc
