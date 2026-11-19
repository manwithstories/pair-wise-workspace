#include "media/recovery.hpp"

#include <algorithm>

namespace media {

RecoveryEngine::RecoveryEngine(const RecoveryConfig& cfg) : cfg_(cfg) {}

RecoveryRecord RecoveryEngine::handle_gap(
    const Gap& gap, const std::vector<Packet>& context) {
  RecoveryRecord rec;
  rec.gap = gap;
  rec.affected_first_frame = static_cast<std::uint32_t>(gap.first_linear);
  rec.affected_last_frame = static_cast<std::uint32_t>(
      gap.first_linear + gap.count - 1);
  ++stats_.attempts;

  const bool audio_gap =
      !context.empty() && context.front().kind == MediaKind::Audio;

  // A span longer than the concealment limit can never be hidden.
  if (gap.count > cfg_.max_recoverable_span) {
    rec.kind = RecoveryKind::Unrecoverable;
    rec.reason = "span exceeds max recoverable length";
    ++stats_.unrecoverable;
    records_.push_back(rec);
    return rec;
  }

  // Video: a missing key frame cannot be rebuilt from an earlier reference.
  if (!audio_gap) {
    for (const auto& p : context) {
      if (p.keyframe &&
          p.linear_index >= gap.first_linear &&
          p.linear_index < gap.first_linear + gap.count) {
        rec.kind = RecoveryKind::Unrecoverable;
        rec.reason = "key frame inside missing span";
        ++stats_.unrecoverable;
        records_.push_back(rec);
        return rec;
      }
    }
  }

  rec.kind = audio_gap ? RecoveryKind::Forward : RecoveryKind::ReferenceFrame;

  // Deterministic, seed-independent failure path for tests and metrics.
  if (cfg_.fail_every_n > 0 &&
      stats_.attempts % cfg_.fail_every_n == 0) {
    rec.success = false;
    rec.reason = "deterministic injected recovery failure";
    ++stats_.failed;
    records_.push_back(rec);
    return rec;
  }

  rec.success = true;
  rec.reason = audio_gap ? "forward PCM interpolation"
                         : "reference frame concealment";
  ++stats_.recovered;
  records_.push_back(rec);
  return rec;
}

std::vector<std::int16_t> RecoveryEngine::forward_fill_audio(
    const std::vector<std::int16_t>& last_block,
    std::uint32_t sample_count) const {
  std::vector<std::int16_t> out;
  out.reserve(sample_count);
  if (last_block.empty()) {
    out.assign(sample_count, 0); // silence when no reference exists
    return out;
  }
  // Linear decay across the gap: the first samples keep the reference level
  // and the tail reaches quarter level, a deterministic click-avoiding fill.
  for (std::uint32_t i = 0; i < sample_count; ++i) {
    const std::int32_t src = last_block[i % last_block.size()];
    const std::int32_t decay =
        1 + 3 * static_cast<std::int32_t>(i) /
            std::max<std::uint32_t>(sample_count - 1, 1);
    out.push_back(static_cast<std::int16_t>(src / decay));
  }
  return out;
}

double recovery_rate(const RecoveryStats& st) {
  if (st.attempts == 0) return 1.0; // nothing lost: perfect by definition
  return static_cast<double>(st.recovered) /
         static_cast<double>(st.attempts);
}

const char* recovery_kind_name(RecoveryKind k) noexcept {
  switch (k) {
    case RecoveryKind::ReferenceFrame: return "reference_frame";
    case RecoveryKind::Forward:        return "forward";
    case RecoveryKind::Unrecoverable:  return "unrecoverable";
    default:                           return "none";
  }
}

} // namespace media
