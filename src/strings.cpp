// Enum <-> string mappings and modular sequence arithmetic (packet model).
#include "media/packet.hpp"

#include <cstdint>

namespace media {

const char* codec_name(CodecType c) noexcept {
  switch (c) {
    case CodecType::H264: return "h264";
    case CodecType::H265: return "h265";
    case CodecType::AV1:  return "av1";
    default:              return "unknown";
  }
}

CodecType codec_from_string(const std::string& s) noexcept {
  if (s == "h264" || s == "H264") return CodecType::H264;
  if (s == "h265" || s == "hevc" || s == "H265") return CodecType::H265;
  if (s == "av1" || s == "AV1") return CodecType::AV1;
  return CodecType::Unknown;
}

const char* media_kind_name(MediaKind k) noexcept {
  return k == MediaKind::Audio ? "audio" : "video";
}

const char* error_code_name(ErrorCode c) noexcept {
  switch (c) {
    case ErrorCode::IndexMissingEntry:     return "index_missing_entry";
    case ErrorCode::IndexOffsetOutOfRange: return "index_offset_out_of_range";
    case ErrorCode::IndexNotMonotonic:     return "index_not_monotonic";
    case ErrorCode::OversizeDeclaration:   return "oversize_declaration";
    case ErrorCode::StreamTruncated:       return "stream_truncated";
    case ErrorCode::CheckpointCorrupt:     return "checkpoint_corrupt";
    case ErrorCode::CheckpointUnrecoverable:
      return "checkpoint_unrecoverable";
    case ErrorCode::PacketUnrecoverable:   return "packet_unrecoverable";
    case ErrorCode::TimestampJump:         return "timestamp_jump";
    case ErrorCode::TimestampMissing:      return "timestamp_missing";
    case ErrorCode::SampleRateChanged:     return "sample_rate_changed";
    case ErrorCode::DuplicateTimestamp:    return "duplicate_timestamp";
    case ErrorCode::SequenceWrap:          return "sequence_wrap";
    default:                               return "none";
  }
}

std::int32_t seq_distance(std::uint16_t a, std::uint16_t b) noexcept {
  // Signed modular difference a-b over the 16-bit sequence space.
  return static_cast<std::int32_t>(static_cast<std::int16_t>(a - b));
}

} // namespace media
