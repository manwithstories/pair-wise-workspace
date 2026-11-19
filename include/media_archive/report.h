#pragma once
// Deterministic terminal reporting. The JSON form contains every field
// required for acceptance; keys are emitted in a fixed order so repeated
// runs produce byte-identical output.

#include <string>

#include "media_archive/types.h"

namespace media_archive {

enum class ReportFormat { Text, Json };

ReportFormat report_format_from_name(const std::string& name);

// Render a report as a single JSON object (no trailing noise).
std::string render_report_json(const RecoveryReport& report);

// Render a human-readable, line-oriented text report.
std::string render_report_text(const RecoveryReport& report);

// Convenience dispatcher.
std::string render_report(const RecoveryReport& report, ReportFormat fmt);

}  // namespace media_archive
