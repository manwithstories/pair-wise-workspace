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
  std::string message;
  Location location;
};

const char* error_code_name(ErrorCode code);
int exit_code_for(ErrorCode code);

bool is_fatal(ErrorCode code);
std::string format_diagnostic(const Diagnostic& diagnostic);
void print_diagnostics(const std::vector<Diagnostic>& diagnostics);

}  // namespace media_restore
