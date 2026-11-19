#include "config.hpp"

#include <sstream>

namespace mediaqc {

std::string Config::summary() const {
  std::ostringstream os;
  os << "v1"
     << ";repair=" << (allow_repair ? 1 : 0)
     << ";rebuild_index=" << (rebuild_index ? 1 : 0)
     << ";keep_decode_errors=" << (keep_decode_errors ? 1 : 0)
     << ";fail_on_unindexable=" << (fail_on_unindexable ? 1 : 0)
     << ";max_memory=" << max_memory_bytes
     << ";max_input=" << limits.max_input_bytes
     << ";max_tracks=" << limits.max_tracks
     << ";max_packet=" << limits.max_packet_bytes;
  return os.str();
}

}  // namespace mediaqc
