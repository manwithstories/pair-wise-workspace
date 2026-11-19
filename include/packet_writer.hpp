#pragma once

// Serialisation helpers used by the generator and by the parser's round-trip
// self-check. Encoding a parsed packet must reproduce the original body bytes
// exactly (canonical metadata ordering is preserved from the input).

#include <span>
#include <vector>

#include "media_types.hpp"

namespace mfs {

std::vector<uint8_t> encode_file_header(const FileHeader& header);
std::vector<uint8_t> encode_packet_body(const Packet& packet);
std::vector<uint8_t> encode_record(const Packet& packet);

}  // namespace mfs
