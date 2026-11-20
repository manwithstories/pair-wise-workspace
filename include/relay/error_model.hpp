#pragma once

// Shared error model: every stage attributes findings to the same severity
// and damage-kind taxonomy so the analysis engine can merge ranges from
// transport, container and caption stages on one timeline.

#include <optional>
#include <string>
#include <vector>

#include "relay/media_time.hpp"

namespace relay {

enum class ErrorSeverity {
  Recoverable,     // pipeline can continue; finding is informational
  Unrecoverable,   // stage cannot continue past this point
  UnknownOrigin,   // cannot be attributed reliably; region is uncertain
};

enum class DamageKind {
  TransportLoss,          // link-layer gap: missing / burst-lost packets
  StructuralCorruption,   // container or bitstream damage in the source
  CaptionAnomaly,         // malformed / overlapping / truncated captions
  Uncertain,              // detected anomaly that resists attribution
};

struct TimeRangeMs {
  Rational start_ms{0, 1};
  Rational end_ms{0, 1};
};

struct DamageRange {
  TimeRangeMs range;
  DamageKind kind = DamageKind::Uncertain;
  ErrorSeverity severity = ErrorSeverity::UnknownOrigin;
  std::string note;  // token-safe (no whitespace) for checkpoint round-trip
};

struct AnalysisError {
  ErrorSeverity severity = ErrorSeverity::UnknownOrigin;
  DamageKind kind = DamageKind::Uncertain;
  std::string message;
  std::optional<TimeRangeMs> range;  // unset when no time anchor exists
};

inline const char* to_string(DamageKind k) {
  switch (k) {
    case DamageKind::TransportLoss: return "transport_loss";
    case DamageKind::StructuralCorruption: return "structural_corruption";
    case DamageKind::CaptionAnomaly: return "caption_anomaly";
    case DamageKind::Uncertain: return "uncertain";
  }
  return "uncertain";
}

inline const char* to_string(ErrorSeverity s) {
  switch (s) {
    case ErrorSeverity::Recoverable: return "recoverable";
    case ErrorSeverity::Unrecoverable: return "unrecoverable";
    case ErrorSeverity::UnknownOrigin: return "unknown_origin";
  }
  return "unknown_origin";
}

inline std::optional<DamageKind> damage_kind_from_string(const std::string& s) {
  if (s == "transport_loss") return DamageKind::TransportLoss;
  if (s == "structural_corruption") return DamageKind::StructuralCorruption;
  if (s == "caption_anomaly") return DamageKind::CaptionAnomaly;
  if (s == "uncertain") return DamageKind::Uncertain;
  return std::nullopt;
}

inline std::optional<ErrorSeverity> severity_from_string(const std::string& s) {
  if (s == "recoverable") return ErrorSeverity::Recoverable;
  if (s == "unrecoverable") return ErrorSeverity::Unrecoverable;
  if (s == "unknown_origin") return ErrorSeverity::UnknownOrigin;
  return std::nullopt;
}

}  // namespace relay
