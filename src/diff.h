#pragma once

#include <string>
#include <vector>

namespace h264 {

struct DiffEntry {
  std::string path;
  std::string left;
  std::string right;
};

// Compares two baseline report JSON documents field by field.
// Volatile sections ("file", "stats") are excluded. Throws kParse when
// either document is not valid JSON.
std::vector<DiffEntry> diff_reports(const std::string& left_json,
                                    const std::string& right_json);

}  // namespace h264
