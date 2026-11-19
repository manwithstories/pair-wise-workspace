#pragma once
// End-to-end recovery pipeline wiring every module together on the shared
// media clock: manifest/stream read -> sequence repair -> timestamp repair
// -> keyframe index (checkpointed) -> audio alignment -> report assembly.

#include <cstdint>
#include <string>

#include "media_archive/checkpoint_store.h"
#include "media_archive/error.h"
#include "media_archive/packet_reader.h"
#include "media_archive/report.h"
#include "media_archive/types.h"

namespace media_archive {

struct RecoveryOptions {
  CodecType codec = CodecType::Unknown;
  std::string input_path;
  std::string packets_path;
  std::string audio_path;
  std::string index_path;
  std::string checkpoint_dir;
  ReportFormat report_format = ReportFormat::Json;
  double drift_ppm = 0.0;
  uint64_t crash_after_index_appends = UINT64_MAX;
  ReaderLimits limits;
  CheckpointConfig checkpoint;
};

struct PipelineResult {
  RecoveryReport report;
  CheckpointOpenResult checkpoint;
  // Filled even on failure so the CLI can print a partial, locatable report.
  bool checkpoint_failure = false;
};

// Run the full recovery. The returned status maps directly to process exit
// codes; `out` always carries whatever report data was gathered.
Diagnostic run_recovery_pipeline(const RecoveryOptions& options,
                                 PipelineResult* out);

// Read back a finished index and verify seek landing error across samples.
Diagnostic verify_index_file(const std::string& index_path, CodecType codec,
                             const std::vector<uint64_t>& sample_pts,
                             double* max_seek_error_ms);

}  // namespace media_archive
