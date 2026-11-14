#pragma once

#include <cstdint>
#include <string>

#include "picture.h"

namespace h264 {

struct SpsSummary {
  bool valid = false;
  uint32_t profile_idc = 0;
  uint32_t level_idc = 0;
  uint32_t width = 0;
  uint32_t height = 0;
  uint32_t chroma_format_idc = 1;
  bool frame_mbs_only_flag = true;
  bool mbaff = false;
  double fps = 0.0;
};

struct Report {
  std::string file;
  uint64_t size_bytes = 0;
  SpsSummary sps;
  StreamStats stream;  // counts, gops, slice histogram, ue stats, warnings
  bool has_stats = false;
  double elapsed_sec = 0.0;
  double peak_rss_mb = 0.0;
};

std::string report_to_json(const Report& r);
std::string report_to_text(const Report& r);

}  // namespace h264
