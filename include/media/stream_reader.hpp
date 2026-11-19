// Raw H.264/H.265/AV1 stream reader, key frame index validation and rebuild.
// Parsing uses the FFmpeg 7/8 stable parser API for H.264/H.265 and a small
// self-contained OBU scanner for AV1, so no demuxer or external file is needed.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/packet.hpp"

namespace media {

struct ReadResult {
  std::vector<Frame> frames;
  std::vector<ErrorEvent> errors;
  StreamIndex validated_index;   // rebuilt entries included
  bool truncated = false;        // stream ended inside a frame/NAL/OBU
  std::uint64_t input_hash = 0;
};

// Codec identification via libavformat's format probing; the probe result must
// agree with the declared codec, otherwise an error event is recorded.
CodecType probe_codec(const std::vector<std::uint8_t>& bytes) noexcept;

// Parse a raw coded stream into frames. When expected_size is non-zero and
// larger than the byte count actually present, the stream is reported as
// truncated (models an interrupted transfer).
ReadResult read_stream(const std::vector<std::uint8_t>& bytes,
                       CodecType declared,
                       const StreamIndex* supplied_index = nullptr,
                       std::uint64_t expected_size = 0);

// Parse the on-disk index text format (see sample_generator::serialize_index).
// Returns false on malformed text; the error detail is written to *error.
bool parse_index_text(const std::string& text, StreamIndex& out,
                      std::string& error);

} // namespace media
