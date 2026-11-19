#include "timeline.hpp"

#include <algorithm>
#include <utility>

namespace mediaqc {
namespace {

constexpr int64_t kMinDuration = 1;

int64_t default_duration(const TimelineOptions& opt) {
  if (opt.estimated_duration > 0) return opt.estimated_duration;
  return kMinDuration;
}

// Largest backwards jump still treated as jitter rather than a discontinuity.
// Two packet durations is a reorder; anything larger is a clock event.
int64_t tolerance(const TimelineOptions& opt, const TimelineState& state) {
  if (opt.ambiguity_tolerance_ticks > 0) return opt.ambiguity_tolerance_ticks;
  if (state.last_duration > 0) return state.last_duration * 2;
  if (!opt.time_base.valid()) return 2;
  return av_rescale_q(2, AVRational{1, 1}, opt.time_base.av());
}

// Adds a repair record, keeping memory bounded.  Returns its index or -1 when
// the cap was reached; the repair is applied either way.
int add_repair(TimelineOptions& opt, TimelineResult& r, RepairKind kind,
               int64_t in_dts, int64_t in_pts, int64_t out_dts, int64_t out_pts,
               int64_t adjustment, int64_t offset, std::string reason) {
  if (static_cast<int64_t>(r.repairs.size()) >= opt.max_repairs) return -1;
  TimestampRepair rep;
  rep.kind = kind;
  rep.input_dts = in_dts;
  rep.input_pts = in_pts;
  rep.output_dts = out_dts;
  rep.output_pts = out_pts;
  rep.adjustment = adjustment;
  rep.byte_offset = offset;
  rep.reason = std::move(reason);
  r.repairs.push_back(std::move(rep));
  return static_cast<int>(r.repairs.size()) - 1;
}

bool fail(TimelineResult& r, const std::string& reason) {
  r.ok = false;
  if (r.failure.empty()) r.failure = reason;
  r.stats.unrepairable++;
  return false;
}

}  // namespace

const char* repair_kind_name(RepairKind k) {
  switch (k) {
    case RepairKind::None: return "none";
    case RepairKind::DtsDerivedFromPts: return "dts_derived_from_pts";
    case RepairKind::PtsDerivedFromDts: return "pts_derived_from_dts";
    case RepairKind::WrapUnwrapped: return "wrap_unwrapped";
    case RepairKind::BackwardsShifted: return "backwards_shifted";
    case RepairKind::DuplicateShifted: return "duplicate_shifted";
  }
  return "unknown";
}

bool timeline_push(TimelineOptions& opt, TimelineResult& r, int64_t src_dts,
                   int64_t src_pts, int64_t duration, int64_t byte_offset,
                   int32_t byte_size, int32_t flags, const SampleSink& sink) {
  if (!r.ok) return false;

  TimelineSample s;
  s.duration = duration > 0 ? duration : 0;
  s.src_dts = src_dts;
  s.src_pts = src_pts;
  s.byte_offset = byte_offset;
  s.byte_size = byte_size;
  s.flags = flags;

  // Fall back to the previous duration (or the estimate) when the container
  // does not carry a per-packet duration.
  if (s.duration <= 0) {
    s.duration = r.state.last_duration > 0 ? r.state.last_duration : default_duration(opt);
  }

  int64_t raw_dts = src_dts;
  RepairKind kind = RepairKind::None;
  std::string reason;
  int64_t adjustment = 0;

  // ---- 1. Missing DTS/PTS -------------------------------------------------
  if (raw_dts == AV_NOPTS_VALUE && src_pts == AV_NOPTS_VALUE) {
    if (!opt.repair_missing) {
      return fail(r, "packet has neither DTS nor PTS and missing-timestamp repair is disabled");
    }
    const int64_t last = r.state.last_out_dts;
    raw_dts = last == AV_NOPTS_VALUE ? 0 : last + s.duration;
    kind = RepairKind::DtsDerivedFromPts;
    reason = "DTS and PTS missing; continued from previous packet duration";
    r.stats.dts_missing++;
  } else if (raw_dts == AV_NOPTS_VALUE) {
    if (!opt.repair_missing) {
      return fail(r, "packet has no DTS and missing-timestamp repair is disabled");
    }
    // Prefer pts - duration; when that would not advance the timeline, place
    // the packet one duration after the previous DTS instead.
    const int64_t last = r.state.last_out_dts;
    int64_t guess = src_pts - s.duration;
    if (last != AV_NOPTS_VALUE && guess <= last) guess = last + s.duration;
    raw_dts = guess;
    kind = RepairKind::DtsDerivedFromPts;
    reason = "DTS missing; derived from PTS and packet duration";
    r.stats.dts_missing++;
  } else if (src_pts == AV_NOPTS_VALUE) {
    kind = RepairKind::PtsDerivedFromDts;
    reason = "PTS missing; derived from DTS";
    r.stats.pts_missing++;
  }

  // ---- 2. Rollover and backwards repair -----------------------------------
  int64_t dts = raw_dts;
  if (r.state.last_out_dts != AV_NOPTS_VALUE) {
    int64_t candidate = raw_dts + r.state.wrap_offset;
    const int64_t last = r.state.last_out_dts;
    if (candidate <= last) {
      const int64_t jump = last - candidate;
      const int64_t period = opt.wrap.period();
      // A rollover requires the previous raw value to sit in the upper half of
      // the period and the jump to exceed the policy threshold.  Ordinary
      // small regressions never qualify.
      const bool upper_half = r.state.last_raw_dts >= period / 2;
      const bool wrap_candidate =
          opt.wrap.enabled && period > 0 && upper_half && jump > opt.wrap.threshold();

      if (wrap_candidate) {
        r.state.wrap_offset += period;
        candidate = raw_dts + r.state.wrap_offset;
        if (candidate <= last) {
          // Land exactly after the last packet, preserving the packet spacing
          // that a correct writer would have produced across the wrap.
          const int64_t extra = last + s.duration - candidate;
          if (extra > 0) {
            r.state.wrap_offset += extra;
            candidate += extra;
          }
          if (candidate <= last) candidate = last + s.duration;
        }
        kind = RepairKind::WrapUnwrapped;
        reason = "DTS rolled over a 32-bit period; unwrapped by one period";
        adjustment = candidate - last;
        r.stats.wrapped++;
      } else if (!opt.repair_backwards) {
        return fail(r, "DTS moved backwards by " + std::to_string(jump) +
                           " ticks and backwards repair is disabled");
      } else if (jump > tolerance(opt, r.state) && opt.reject_ambiguous) {
        // A large jump backwards that is not a rollover may be a duplicated
        // span or a clock reset.  Retiming it would move media across the
        // timeline, so the asset is failed instead of guessed at.
        return fail(r, "DTS moved backwards by " + std::to_string(jump) +
                           " ticks; neither a 32-bit rollover nor a bounded glitch");
      } else {
        // A bounded glitch.  A DTS that merely repeats the previous value is
        // harmless and left untouched (codecs that reorder do this).  A DTS
        // that moves backwards is only repairable when the whole track has
        // not been shifted before; after the first shift, a second backwards
        // move would make the accumulated retiming impossible to distinguish
        // from a genuine discontinuity, so the track fails instead.
        if (jump == 0) {
          kind = RepairKind::DuplicateShifted;
          reason = "duplicate DTS left in place; it is already monotonic";
          adjustment = 0;
          r.stats.duplicates++;
        } else if (r.state.shift != 0) {
          return fail(r, "DTS moved backwards a second time after a repair; the "
                         "accumulated retiming would be ambiguous");
        } else {
          const int64_t delta = last - candidate;
          r.state.wrap_offset += delta;
          r.state.shift += delta;
          candidate = last;
          kind = RepairKind::BackwardsShifted;
          reason = "backwards DTS clamped by " + std::to_string(delta) +
                   " ticks; this is the only such repair for the track";
          adjustment = delta;
          r.stats.backwards_shifted++;
        }
      }
    }
    dts = candidate;
  }

  // ---- 3. PTS -------------------------------------------------------------
  // The PTS is shifted by exactly the amount the DTS was shifted from its raw
  // value, which preserves each frame's decode/presentation offset.  When the
  // DTS was itself derived from the PTS, the PTS is left untouched.
  int64_t pts = src_pts;
  if (src_pts == AV_NOPTS_VALUE) {
    pts = dts;
  } else if (src_dts != AV_NOPTS_VALUE) {
    pts = src_pts + (dts - src_dts);
  }
  if (pts < dts) pts = dts;

  s.dts = dts;
  s.pts = pts;

  // ---- 4. Bookkeeping -----------------------------------------------------
  if (kind != RepairKind::None) {
    s.repair_index = add_repair(opt, r, kind, src_dts, src_pts, dts, pts,
                                dts - (src_dts == AV_NOPTS_VALUE ? dts : src_dts),
                                byte_offset, reason);
  }

  const int64_t gap_threshold =
      opt.gap_threshold_ticks > 0 ? opt.gap_threshold_ticks : opt.wrap.period() / 8;
  if (r.state.packets > 0 && gap_threshold > 0) {
    const int64_t step = dts - r.state.last_out_dts;
    if (step > gap_threshold) r.gap_count++;
  }

  r.state.last_out_dts = dts;
  r.state.last_raw_dts = raw_dts;
  r.state.last_pts = pts;
  r.state.last_duration = s.duration;
  r.stats.packets++;
  r.state.packets++;
  r.stats.total_adjustment += adjustment;
  if (r.stats.first_input_offset < 0) r.stats.first_input_offset = byte_offset;
  if (r.first_out_dts == AV_NOPTS_VALUE) {
    r.first_out_dts = dts;
    r.first_out_pts = pts;
  }
  if (opt.retain_samples) r.samples.push_back(s);
  if (sink) sink(s);
  return true;
}

void timeline_set_failure_range(TimelineResult& r, int64_t offset, int64_t size) {
  if (offset < 0) return;
  r.failure_offset = offset;
  r.failure_offset_end = offset + std::max<int64_t>(0, size);
}

void timeline_align_starts(std::vector<TimelineResult*>& tracks) {
  int64_t min_start = AV_NOPTS_VALUE;
  for (const auto* t : tracks) {
    if (!t->ok || t->stats.packets == 0) continue;
    const int64_t start =
        t->first_out_pts != AV_NOPTS_VALUE ? t->first_out_pts : t->first_out_dts;
    if (start == AV_NOPTS_VALUE) continue;
    if (min_start == AV_NOPTS_VALUE || start < min_start) min_start = start;
  }
  if (min_start == AV_NOPTS_VALUE) return;
  for (auto* t : tracks) {
    if (!t->ok || t->stats.packets == 0) continue;
    t->start_shift = -min_start;
  }
}

int64_t timeline_to_canonical(const TimelineResult& track, int64_t ticks) {
  if (ticks == AV_NOPTS_VALUE) return AV_NOPTS_VALUE;
  const int64_t shifted = ticks + track.start_shift;
  return rescale_ticks(shifted, track.time_base, Rational::from_av(kCanonicalBase));
}

int64_t timeline_first_canonical_pts(const TimelineResult& track) {
  if (track.first_out_pts == AV_NOPTS_VALUE) return AV_NOPTS_VALUE;
  return timeline_to_canonical(track, track.first_out_pts);
}

}  // namespace mediaqc
