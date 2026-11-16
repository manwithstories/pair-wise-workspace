#pragma once

#include "gop_builder.h"

#include <iosfwd>
#include <string>
#include <vector>

namespace h264 {

void printProbe(std::ostream& out, const FileAnalysis& analysis);
void printIndexSummary(std::ostream& out, const std::vector<FileAnalysis>& analyses,
                       const std::string& jsonPath);
bool writeIndexJson(const std::string& path, const std::vector<FileAnalysis>& analyses,
                    std::string& error);

}  // namespace h264
