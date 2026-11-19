// Loss recovery and unrecoverable-error classification.
// Video gaps are recovered from a reference frame when the span is short and
// no key frame is inside it (reference-frame concealment); longer spans or
// gaps containing an IDR are classified unrecoverable. Audio gaps use forward
// interpolation of PCM samples. Every attempt records its outcome and the
// frame range it affected.
#pragma once

#include <cstdint>
#include <vector>

#include "media/jitter_buffer.hpp"
#include "media/packet.hpp"

namespace media {

enum class RecoveryKind : std::uint8_t {
  None = 0,
  ReferenceFrame = 1, // video concealment against the last key/reference frame
  Forward = 2,        // audio forward interpolation
  Unrecoverable = 3,
};

struct RecoveryRecord {
  Gap gap;
  RecoveryKind kind = RecoveryKind::None;
  bool success = false;
  std::uint32_t affected_first_frame = 0;
  std::uint32_t affected_last_frame = 0;
  std::string reason;
};

struct RecoveryConfig {
  std::uint32_t max_recoverable_span = 8; // packets; beyond this is fatal
  // Deterministic failure injection: every Nth recoverable attempt fails so
  // the failure path and recovery_rate < 1 are both exercised reproducibly.
  std::uint32_t fail_every_n = 0;
};

struct RecoveryStats {
  std::uint64_t attempts = 0;
  std::uint64_t recovered = 0;
  std::uint64_t failed = 0;
  std::uint64_t unrecoverable = 0;
};

class RecoveryEngine {
 public:
  explicit RecoveryEngine(const RecoveryConfig& cfg = {});

  // Decide and record the outcome for one gap. The surrounding packets let
  // the engine see whether the missing span contains a key frame.
  RecoveryRecord handle_gap(const Gap& gap,
                            const std::vector<Packet>& context);

  // Synthesize PCM samples that conceal an audio block gap.
  std::vector<std::int16_t> forward_fill_audio(
      const std::vector<std::int16_t>& last_block,
      std::uint32_t sample_count) const;

  const RecoveryStats& stats() const noexcept { return stats_; }
  const std::vector<RecoveryRecord>& records() const noexcept {
    return records_;
  }

 private:
  RecoveryConfig cfg_;
  RecoveryStats stats_;
  std::vector<RecoveryRecord> records_;
};

// recovery_rate = recovered / attempts over the supplied records.
double recovery_rate(const RecoveryStats& st);

const char* recovery_kind_name(RecoveryKind k) noexcept;

} // namespace media
