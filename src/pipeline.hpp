// pipeline.hpp -- per-asset and batch orchestration.
//
// One asset flows through: scan (structure + packets) -> timeline repair ->
// journal append -> index-rebuilding mux -> independent verification ->
// atomic rename.  Checkpoints are written between batch windows so an
// interrupted run resumes from the committed packet counts rather than
// rewriting the output.  Failures never touch the existing target file and
// never stop the rest of the batch.
#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "config.hpp"
#include "index_writer.hpp"
#include "media_error.hpp"

namespace mediaqc {

enum class FileStatus {
  Ok,           // output produced and verified
  Failed,       // asset could not be repaired/verified; no output installed
  Interrupted,  // stopped intentionally (testing aid) with a resumable checkpoint
  Resumed,      // previous verified output reused via checkpoint
};

const char* file_status_name(FileStatus s);

struct FileResult {
  std::string input_path;
  std::string output_path;
  FileStatus status = FileStatus::Failed;
  std::string input_sha256;
  std::string output_sha256;
  int64_t input_bytes = 0;
  int64_t output_bytes = 0;
  int64_t packets = 0;
  int64_t index_entries = 0;
  int64_t keyframes = 0;
  int64_t decode_errors = 0;
  int64_t repairs = 0;
  int64_t dropped_packets = 0;
  int64_t first_error_offset = -1;   // damage found in this asset's output
  int64_t offset_error_bytes = 0;
  // Input-side evidence carried from the sidecar (verify mode): where the
  // input was damaged, regardless of whether the output still decodes.
  int64_t input_evidence_offset = -1;
  int64_t input_evidence_uncertainty = 0;
  int64_t resume_seconds = 0;
  bool resumed = false;
  std::string journal_path;  // byte-offset map kept for the verify subcommand
  bool checkpoint_refused = false;
  std::string refusal_reason;
  std::vector<Error> errors;
  std::vector<UnindexableRange> unindexable;
  std::string failure_reason;
};

struct BatchOptions {
  Config config;
  std::filesystem::path input_dir;
  std::filesystem::path output_dir;
  std::filesystem::path checkpoint_dir;  // empty -> output_dir/.mediaqc
  bool resume = false;
  bool json = false;
  bool verbose = false;
  // Testing aid: stop each asset after N packets, leaving a checkpoint.
  int64_t stop_after_packets = 0;
  int64_t checkpoint_interval_packets = 256;
};

struct BatchTotals {
  int64_t files_total = 0;
  int64_t files_ok = 0;
  int64_t files_failed = 0;
  int64_t files_resumed = 0;
  int64_t files_interrupted = 0;
  int64_t checkpoint_refusals = 0;
  int64_t first_error_offset = -1;   // damage found in this asset's output
  int64_t offset_error_bytes = 0;
  // Input-side evidence carried from the sidecar (verify mode): where the
  // input was damaged, regardless of whether the output still decodes.
  int64_t input_evidence_offset = -1;
  int64_t input_evidence_uncertainty = 0;
  int64_t resume_seconds = 0;
  int64_t index_entries = 0;
  int64_t decode_errors = 0;
  int64_t repairs = 0;
  int64_t packets = 0;
  std::string output_sha256;  // digest over sorted per-file output hashes
};

struct BatchResult {
  BatchTotals totals;
  std::vector<FileResult> files;
  int exit_code = 0;
  int64_t elapsed_seconds = 0;
  std::string error;  // configuration / setup error (exit code 2)
};

// Discovers media files under input_dir, rejecting entries that resolve
// outside the directory.
std::vector<std::filesystem::path> discover_inputs(const std::filesystem::path& input_dir,
                                                   std::vector<Error>& rejected);

BatchResult run_batch(const BatchOptions& options);

// verify subcommand: re-verifies every output in a directory.  Input-offset
// evidence is loaded from the `<output>.qc.json` sidecars written by batch.
struct VerifyOptions {
  Config config;
  std::filesystem::path input_dir;
  bool json = false;
  bool verbose = false;
};
BatchResult run_verify(const VerifyOptions& options);

struct SelftestOptions {
  Config config;
  std::filesystem::path work_dir;
  std::string ffmpeg_binary = "ffmpeg";
  bool json = false;
};
BatchResult run_selftest(const SelftestOptions& options);

// Renders batch/verify results as one-line JSON (keys fixed for the demo).
std::string render_json(const BatchResult& result, bool verify_mode);

}  // namespace mediaqc
