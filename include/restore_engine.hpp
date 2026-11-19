#pragma once

#include <cstdint>
#include <filesystem>
#include <map>
#include <optional>
#include <string>
#include <vector>

#include "audio_restore.hpp"
#include "bitstream.hpp"
#include "color_metadata.hpp"
#include "error.hpp"
#include "json.hpp"
#include "manifest.hpp"
#include "timeline.hpp"

namespace media_restore {

struct RestoreOptions {
  int rate = 48000;
  std::string layout = "5.1";
  double loudness = -23.0;
  double true_peak = -1.0;
  bool loudness_enabled = true;
  ColorStrategy color_strategy = ColorStrategy::archive;
  ArchiveColor archive_color;
  std::filesystem::path output_dir;  // empty: no media written
  bool write_media = false;
};

// One segment after validation and (optionally) restoration.
struct SegmentOutcome {
  std::string track_id;
  std::string segment_id;
  bool quarantined = false;          // manifest declared an expected failure
  bool validated = false;            // bitstream/index checks passed
  bool restored = false;
  bool skipped = false;              // dependency failed
  std::optional<ErrorCode> failure;  // first fatal error for this segment
  std::string failure_message;
  size_t index_entries = 0;
  size_t index_hits = 0;
  size_t index_generated = 0;
  size_t scan_keyframes = 0;
  int64_t stream_bytes = 0;
  std::string segment_hash;          // stable SHA-256 of the written payload
  bool hash_reused = false;          // preserved from a previous valid result
  std::string output_path;
  std::string summary_hash;          // hash of the processing summary (no media)
  AudioStats audio;
  std::vector<Diagnostic> diagnostics;
};

struct RestoreSummary {
  std::vector<std::string> track_order;
  std::vector<std::string> cyclic_tracks;
  std::vector<std::string> blocked_tracks;
  std::vector<SegmentOutcome> segments;
  size_t healthy_segments = 0;
  size_t healthy_hash_matches = 0;
  size_t quarantined_ok = 0;
  size_t quarantined_mismatch = 0;
  size_t failures = 0;
  int64_t total_segments = 0;
  Rational time_base;
};

// Validates the archive and optionally restores audio.  `options.write_media`
// controls whether WAV payloads are written next to the result.
bool restore_archive(const Manifest& manifest, const RestoreOptions& options,
                     const Limits& limits, RestoreSummary* summary,
                     std::vector<Diagnostic>* diagnostics);

// Builds the machine-readable result document.  Kept separate so `restore`
// can write JSON and `verify` can compare against it.
Json restore_result_json(const Manifest& manifest, const RestoreSummary& summary,
                         const RestoreOptions& options);

struct VerifyOptions {
  double loudness_target = -23.0;
  double loudness_tolerance = 1.0;
  int expected_rate = 0;  // 0: take from the result document
  std::string expected_layout;
};

struct VerifySummary {
  size_t manifest_segments = 0;
  size_t manifest_roundtripped = 0;
  size_t keyframe_hits = 0;
  size_t keyframe_total = 0;
  size_t hash_matches = 0;
  size_t hash_total = 0;
  size_t quarantined_ok = 0;
  size_t quarantined_expected = 0;
  int audio_rate_out = 0;
  std::string layout_out;
  double loudness_lufs = 0.0;
  bool loudness_checked = false;
  size_t loudness_samples = 0;
  double loudness_sum = 0.0;
  size_t failures = 0;
};

// Re-checks a result document against the archive: hashes, index hits,
// quarantine expectations and the audio summary.  Never trusts the result
// blindly; hashes are recomputed from the files on disk.
bool verify_archive(const Manifest& manifest, const Json& result,
                    const VerifyOptions& options, const Limits& limits,
                    VerifySummary* summary,
                    std::vector<Diagnostic>* diagnostics);

// Pure-manifest round trip check used by inspect/verify output.
size_t manifest_roundtrip_score(const std::filesystem::path& path,
                               const Manifest& manifest);

}  // namespace media_restore
