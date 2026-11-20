#pragma once

#include <string>

#include "relay/frame_reader.hpp"
#include "relay/packet_reorder.hpp"

namespace relay {

/// Serialises the verified frame catalog and arrival schedule so a resumed
/// run can skip SHA-256 verification and FFmpeg decoding entirely.  The
/// payload bytes are released with the first run, so the cache stays small.
std::string serialize_read_cache(const ReadResult& result);

bool parse_read_cache(const std::string& text, ReadResult& out,
                      std::string& error);

/// Serialises reordered streams (frame metadata only, no payload bytes).
std::string serialize_reorder_cache(const std::vector<TrackStream>& streams);

bool parse_reorder_cache(const std::string& text,
                         std::vector<TrackStream>& out, std::string& error);

}  // namespace relay
