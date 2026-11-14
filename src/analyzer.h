#pragma once

#include <string>

#include "report.h"

namespace h264 {

// Runs the full pipeline: annexb scan -> nal/sps/pps/slice/sei decode ->
// picture/GOP aggregation. Throws Error(kParse|kUnsupported) on stream
// problems, std::runtime_error on I/O failure.
Report analyze_file(const std::string& path);

// Peak resident set size of this process, in MiB.
double peak_rss_mb();

}  // namespace h264
