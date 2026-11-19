#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace media_restore {

// Stable, machine-readable error codes.  The spelling is part of the CLI
// contract (`code=E_...`), so codes must not be renamed casually.
enum class ErrorCode {
  ok,
  usage,
  io,
  json_parse,
  manifest_schema,
  unknown_codec,
  index_version,
  index_negative_offset,
  index_out_of_range,
  index_duplicate,
  index_order,
  index_not_keyframe,
  index_too_large,
  segment_overlap,
  segment_empty,
  segment_negative,
  dependency_cycle,
  dependency_failed,
  audio_unsupported_layout,
  audio_unsupported_rate,
  audio_decode,
  audio_too_large,
  loudness_invalid,
  color_conflict,
  hash_mismatch,
  limit_exceeded,
  internal,
};

// Errors determine the process exit code; warnings are reported but do not
// fail a run (for example a negative audio offset that is compensated).
enum class Severity { error, warning };

// Where a diagnostic happened.  Media time is expressed in integer ticks of
// the manifest time base; byte offsets always refer to the named file.
struct Location {
  std::string file;
  int64_t byte_offset = -1;
  int64_t tick = -1;
  std::string track;
  std::string segment;
  std::string field;
};

struct Diagnostic {
  ErrorCode code = ErrorCode::ok;
  Severity severity = Severity::error;
  std::string message;
  Location location;

  bool is_error() const { return severity == Severity::error && code != ErrorCode::ok; }
};

// Highest exit code among error diagnostics (0 when there are none).
int exit_code_for(const std::vector<Diagnostic>& diagnostics);

const char* error_code_name(ErrorCode code);
// Parses the "E_..." spelling used in manifests and JSON output.
bool parse_error_code(std::string_view name, ErrorCode* out);
int exit_code_for(ErrorCode code);
const char* severity_name(Severity severity);

bool is_fatal(ErrorCode code);
std::string format_diagnostic(const Diagnostic& diagnostic);
void print_diagnostics(const std::vector<Diagnostic>& diagnostics);

}  // namespace media_restore
