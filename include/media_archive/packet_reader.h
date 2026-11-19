#pragma once
// Raw elementary stream parsing (H.264 / H.265 / AV1) plus the transport
// packet manifest. The codec must be named explicitly — the reader never
// guesses a format, and an unrecognized stream fails with a byte offset.

#include <cstdint>
#include <string>
#include <vector>

#include "media_archive/error.h"
#include "media_archive/types.h"

namespace media_archive {

struct ParsedFrame {
  uint64_t source_offset = 0;  // byte offset inside the input file
  uint32_t size_bytes = 0;
  uint32_t frame_type = 0;     // AVPictureType value, 0 if unknown
  bool keyframe = false;
};

struct ReaderLimits {
  uint64_t max_input_bytes = kDefaultMaxInputBytes;
  uint64_t max_index_offset = kDefaultMaxInputBytes;
  uint32_t max_block_bytes = kMaxBlockBytes;
};

// Read a whole file subject to the size limit. Failure carries the
// offending size/offset in the diagnostic.
Diagnostic read_file_bounded(const std::string& path,
                             const ReaderLimits& limits,
                             std::vector<uint8_t>* out);

// Parse a raw elementary stream. `codec` must be one of H264/H265/AV1;
// Unknown is rejected. On parser failure the diagnostic offset points at
// the first byte the codec parser could not consume.
Diagnostic parse_elementary_stream(const std::vector<uint8_t>& bytes,
                                   CodecType codec,
                                   const ReaderLimits& limits,
                                   std::vector<ParsedFrame>* frames);

// Parse the packet manifest (CSV header:
// seq,pts,offset,size,flag,send_ms,recv_ms). Gap rows carry offset == -1.
Diagnostic parse_packet_manifest(const std::string& path,
                                 std::vector<TransportPacket>* packets);

// Serialize a manifest back to CSV (used by the sample generator).
Diagnostic write_packet_manifest(const std::string& path,
                                 const std::vector<TransportPacket>& packets);

const char* packet_flag_name(PacketFlag flag);
PacketFlag packet_flag_from_name(const std::string& name);

}  // namespace media_archive
