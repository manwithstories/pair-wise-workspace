#pragma once
// Random access index rebuilt from recovered keyframes. Every record holds
// the repaired PTS on the shared clock, the source byte offset, the frame
// type and a CRC32 of the encoded bytes. The index is serialized as a small
// binary archive and seek verification proves the landing-point error.

#include <cstdint>
#include <string>
#include <vector>

#include "media_archive/error.h"
#include "media_archive/packet_reader.h"
#include "media_archive/types.h"

namespace media_archive {

struct IndexFileHeader {
  char magic[8];          // "MAKIDX1\0"
  uint32_t codec = 0;     // CodecType value
  uint32_t record_count = 0;
  uint64_t stream_size = 0;
  uint32_t checksum = 0;  // over every serialized record
};

// Build entries from parsed frames plus their repaired PTS and extended
// sequence numbers. `stream` supplies the bytes used for the CRC checksum.
// Only keyframes are indexed; entries stay sorted by PTS.
std::vector<KeyframeEntry> build_keyframe_index(
    const std::vector<ParsedFrame>& frames,
    const std::vector<RepairedPacket>& repaired,
    const std::vector<uint8_t>& stream,
    uint64_t stream_size);

// Nearest-keyframe-at-or-before lookup, matching random-access semantics.
SeekCheck verify_seek(const std::vector<KeyframeEntry>& entries,
                      uint64_t requested_pts);

// Binary serialization with a header checksum; corruption is reported with
// an offset or the failing record index.
Diagnostic write_keyframe_index(const std::string& path, CodecType codec,
                                const std::vector<KeyframeEntry>& entries,
                                uint64_t stream_size);
Diagnostic read_keyframe_index(const std::string& path, CodecType expected,
                               std::vector<KeyframeEntry>* entries,
                               IndexFileHeader* header_out);

}  // namespace media_archive
