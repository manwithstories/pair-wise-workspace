#include "media/cli.hpp"
#include "media/packet.hpp"

#include <cstdio>

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

int run_cli(const std::vector<std::string>& args) {
  if (args.empty() || args[0] == "--help" || args[0] == "-h") {
    std::printf("media_replay: demo | resume | fixtures (see README in --help)\n");
    return args.empty() ? 64 : 0;
  }
  std::fprintf(stderr, "error: command '%s' not implemented yet\n",
               args[0].c_str());
  return 64;
}

} // namespace media
