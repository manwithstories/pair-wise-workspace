// config.hpp -- limits, pipeline configuration and its digest.
#pragma once

#include <cstdint>
#include <string>

namespace mediaqc {

struct Limits {
  int64_t max_input_bytes = int64_t{16} * 1024 * 1024 * 1024;  // 16 GiB per asset
  int max_tracks = 64;
  int64_t max_packet_bytes = 64 * 1024 * 1024;
  int64_t max_timeline_errors = 100000;  // per asset
  int64_t max_offset_errors = 100000;
  int max_decode_errors = 4096;
};

struct Config {
  // Structural escapes are resolved against this root; anything outside is
  // rejected before the file is opened.
  std::string input_root;
  std::string output_root;
  bool allow_repair = true;       // attempt DTS/PTS repair
  bool rebuild_index = true;      // rebuild when moov/stco is unusable
  bool keep_decode_errors = true; // report decode errors without dropping
  bool fail_on_unindexable = false;
  int64_t max_memory_bytes = int64_t{256} * 1024 * 1024;
  Limits limits;

  // Summary fed into the resume checkpoint.  Two runs resume each other only
  // when this string matches exactly.
  std::string summary() const;
};

}  // namespace mediaqc
