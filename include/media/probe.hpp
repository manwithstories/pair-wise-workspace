#pragma once
// Container probing: direct RIFF/WAV header walk (byte-offset errors),
// libavformat packet analysis and index-page sidecar handling.
#include <cstdint>
#include <functional>
#include <string>
#include <vector>

#include "media/timeline.hpp"

namespace mediaqc {

struct WavHeader {
    int audio_format = 0;        // 1 = PCM, 0xFFFE = EXTENSIBLE
    int channels = 0;
    int sample_rate = 0;
    int bits_per_sample = 0;
    int block_align = 0;
    int64_t byte_rate = 0;
    int64_t data_offset = -1;    // byte offset of the data chunk body
    int64_t declared_data_size = -1;  // size recorded in the chunk header
    int64_t available_data_size = -1;  // bytes from body start to physical EOF
    int64_t actual_data_size = -1;  // whole-frame bytes usable for decoding
    bool riff_ok = false;
    bool data_chunk_found = false;
};

struct IndexPage {
    int page_no = 0;
    int64_t offset = 0;          // source byte offset of the page
    int64_t size = 0;            // byte length
    int64_t start_sample = 0;    // first sample frame covered
    int64_t end_sample = 0;      // one past the last covered frame
    uint32_t crc32 = 0;
};

struct PacketInfo {
    int64_t source_offset = -1;  // pkt.pos in the source file
    int64_t pts_samples = 0;     // packet start in source sample frames
    int frame_samples = 0;       // frames contained in the packet
    int64_t duration_ns = 0;
    bool keyframe = false;
};

struct ProbeResult {
    std::string path;
    int64_t file_size = 0;
    WavHeader header;
    std::vector<IndexPage> pages;
    bool index_present = false;
    bool index_rebuilt = false;
    std::vector<DamageRecord> errors;
    int64_t packet_count = 0;
    int64_t total_frames = 0;
};

// Parse the RIFF/WAV header directly. Every structural problem is recorded
// with the exact source byte offset at which it was observed.
ProbeResult probe_wav_header(const std::string& path);

// Load and validate the "<path>.idx.json" sidecar. Missing sidecar records a
// MissingIndex error; a broken/non-contiguous sidecar records IndexGap.
// On success result.pages is populated.
void load_index(ProbeResult& result);

// Rebuild index pages by sequential scan from the data chunk start (the last
// decodable boundary). Returns false when no decodable boundary exists.
bool rebuild_index(ProbeResult& result, int frames_per_page);

// Stream every packet through the callback without retaining packet data, so
// memory stays bounded regardless of stream duration. A libavformat open
// failure appends an error (offset 0) and returns false.
using PacketCallback = std::function<void(const PacketInfo&)>;
bool stream_packets(const std::string& path, const WavHeader& header,
                    const PacketCallback& cb, int64_t& packet_count,
                    int64_t& total_frames, std::vector<DamageRecord>& errors);

// CRC of one byte range of the source file (streamed, bounded memory).
uint32_t file_range_crc(const std::string& path, int64_t offset, int64_t size);

}  // namespace mediaqc
