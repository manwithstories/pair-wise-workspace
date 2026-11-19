#include "error.hpp"

#include <iostream>
#include <sstream>

namespace media_restore {
namespace {

struct CodeInfo {
  ErrorCode code;
  const char* name;
  int exit_code;
};

// Exit codes follow the CLI contract:
//   1 usage / input, 2 media validation, 3 restore or dependency failure.
constexpr CodeInfo kCodes[] = {
    {ErrorCode::ok, "E_OK", 0},
    {ErrorCode::usage, "E_USAGE", 1},
    {ErrorCode::io, "E_IO", 1},
    {ErrorCode::json_parse, "E_JSON_PARSE", 1},
    {ErrorCode::manifest_schema, "E_MANIFEST_SCHEMA", 1},
    {ErrorCode::unknown_codec, "E_UNKNOWN_CODEC", 2},
    {ErrorCode::index_version, "E_INDEX_VERSION", 2},
    {ErrorCode::index_negative_offset, "E_INDEX_NEGATIVE_OFFSET", 2},
    {ErrorCode::index_out_of_range, "E_INDEX_OUT_OF_RANGE", 2},
    {ErrorCode::index_duplicate, "E_INDEX_DUPLICATE", 2},
    {ErrorCode::index_order, "E_INDEX_ORDER", 2},
    {ErrorCode::index_not_keyframe, "E_INDEX_NOT_KEYFRAME", 2},
    {ErrorCode::index_too_large, "E_INDEX_TOO_LARGE", 2},
    {ErrorCode::segment_overlap, "E_SEGMENT_OVERLAP", 2},
    {ErrorCode::segment_empty, "E_SEGMENT_EMPTY", 2},
    {ErrorCode::segment_negative, "E_SEGMENT_NEGATIVE", 2},
    {ErrorCode::dependency_cycle, "E_DEPENDENCY_CYCLE", 3},
    {ErrorCode::dependency_failed, "E_DEPENDENCY_FAILED", 3},
    {ErrorCode::audio_unsupported_layout, "E_AUDIO_UNSUPPORTED_LAYOUT", 3},
    {ErrorCode::audio_unsupported_rate, "E_AUDIO_UNSUPPORTED_RATE", 3},
    {ErrorCode::audio_decode, "E_AUDIO_DECODE", 3},
    {ErrorCode::audio_too_large, "E_AUDIO_TOO_LARGE", 3},
    {ErrorCode::loudness_invalid, "E_LOUDNESS_INVALID", 3},
    {ErrorCode::color_conflict, "E_COLOR_CONFLICT", 3},
    {ErrorCode::hash_mismatch, "E_HASH_MISMATCH", 2},
    {ErrorCode::limit_exceeded, "E_LIMIT_EXCEEDED", 2},
    {ErrorCode::internal, "E_INTERNAL", 3},
};

const CodeInfo* find(ErrorCode code) {
  for (const auto& entry : kCodes) {
    if (entry.code == code) return &entry;
  }
  return nullptr;
}

}  // namespace

const char* error_code_name(ErrorCode code) {
  const CodeInfo* info = find(code);
  return info ? info->name : "E_UNKNOWN";
}

bool parse_error_code(std::string_view name, ErrorCode* out) {
  for (const auto& entry : kCodes) {
    if (name == entry.name) {
      *out = entry.code;
      return true;
    }
  }
  return false;
}

const char* severity_name(Severity severity) {
  return severity == Severity::error ? "error" : "warning";
}

int exit_code_for(ErrorCode code) {
  const CodeInfo* info = find(code);
  return info ? info->exit_code : 3;
}

int exit_code_for(const std::vector<Diagnostic>& diagnostics) {
  int code = 0;
  for (const auto& diagnostic : diagnostics) {
    if (!diagnostic.is_error()) continue;
    const int candidate = exit_code_for(diagnostic.code);
    if (candidate > code) code = candidate;
  }
  return code;
}

bool is_fatal(ErrorCode code) {
  return exit_code_for(code) != 0;
}

std::string format_diagnostic(const Diagnostic& diagnostic) {
  std::ostringstream out;
  out << severity_name(diagnostic.severity) << " code="
      << error_code_name(diagnostic.code);
  const Location& loc = diagnostic.location;
  if (!loc.track.empty()) out << " track=" << loc.track;
  if (!loc.segment.empty()) out << " segment=" << loc.segment;
  if (!loc.file.empty()) out << " file=" << loc.file;
  if (loc.byte_offset >= 0) out << " offset=" << loc.byte_offset;
  if (loc.tick >= 0) out << " tick=" << loc.tick;
  if (!loc.field.empty()) out << " field=" << loc.field;
  out << " message=" << diagnostic.message;
  return out.str();
}

void print_diagnostics(const std::vector<Diagnostic>& diagnostics) {
  for (const auto& diagnostic : diagnostics) {
    std::cerr << format_diagnostic(diagnostic) << '\n';
  }
}

}  // namespace media_restore
