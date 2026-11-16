#pragma once
#include <string>
#include "lut/LutTable.h"

namespace lg {

struct LutInfo {
  LutTable table;
  std::string format; // cube | 3dl | spi3d
  std::string order;  // channel traversal order in the source file
};

// Throws LutError (exit 3) on malformed content, IoError (exit 4) on
// missing/unreadable files. Tolerates comments, blank lines, CRLF and
// scientific notation.
LutInfo parseLut(const std::string& path);

} // namespace lg
