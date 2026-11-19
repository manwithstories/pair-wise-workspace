// Deterministic in-memory sample generator: synthetic raw H.264/H.265/AV1
// streams, PCM audio blocks, key frame indexes and damaged fixtures.
// No external files are referenced; every byte derives from a fixed seed.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/packet.hpp"

namespace media {

struct SampleConfig {
  CodecType codec = CodecType::H264;
  std::uint32_t seed = 41;
  std::uint32_t frame_count = 120;
  std::uint32_t gop = 30;                 // IDR every N frames
  std::uint32_t fps_num = 30;            // 30 fps
  std::uint32_t fps_den = 1;
  std::uint32_t audio_sample_rate = 48'000;
  std::uint32_t audio_block_samples = 960; // 20 ms at 48 kHz
  bool truncated = false;                // cut the last frame in half
};

struct SampleBundle {
  std::vector<std::uint8_t> video;       // raw coded stream bytes
  std::vector<Frame> frames;             // ground-truth frame boundaries
  std::vector<AudioBlock> audio;
  StreamIndex index;                     // valid key frame index
  std::uint64_t input_hash = 0;          // FNV-1a of the full video bytes
};

// Build the complete deterministic bundle described by cfg.
SampleBundle generate_sample(const SampleConfig& cfg);

// Re-serialize an index in the on-disk text format (see stream_reader.hpp).
std::string serialize_index(const StreamIndex& idx);

// ---- Damaged fixtures (feature 8) -------------------------------------------

// Clone the given index and move one offset past the end of the stream.
StreamIndex make_oversize_index(const SampleBundle& b);

// Drop one key frame entry to force a scan-and-rebuild path.
StreamIndex make_missing_entry_index(const SampleBundle& b);

// Reorder two entries so key frame timestamps are no longer monotonic.
StreamIndex make_nonmonotonic_index(const SampleBundle& b);

// Return a copy of the video bytes truncated inside the final frame.
std::vector<std::uint8_t> make_truncated_stream(const SampleBundle& b);

// Serialize an index file whose declared size exceeds the safety limit.
std::string make_oversize_declaration(const SampleBundle& b);

} // namespace media
