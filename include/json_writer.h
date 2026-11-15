#pragma once

#include "gop_report.h"

#include <string>
#include <vector>

namespace h264scan {

bool writeJsonReports(const std::vector<FileReport>& reports, const std::string& path,
                      std::string& error);
bool writeCsvReports(const std::vector<FileReport>& reports, const std::string& path,
                     std::string& error);

}  // namespace h264scan
