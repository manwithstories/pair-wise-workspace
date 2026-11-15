#pragma once

#include "gop_report.h"

#include <string>

namespace h264scan {

// Returns 0 on success, 1 for strict-mode warnings, 2 for I/O corruption, and
// 3 when no valid SPS was found. The report is populated whenever possible.
int analyzeFile(const std::string& path, bool strict, FileReport& report,
                std::string& error);

}  // namespace h264scan
