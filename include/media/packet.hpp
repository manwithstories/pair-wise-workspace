// Core media packet, time base and error model shared by every module.
// All timestamps use a single virtual media clock in microseconds.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace media {

// A rational time base. pts_us = pts * num / den is only used by helpers;
// the pipeline itself keeps every timestamp in microseconds.
struct Rational {
  int num = 1;
  int den = 1'000'000;
};

enum class CodecType : std::uint8_t {
  Unknown = 0,
  H264 = 1,
  H265 = 2,
  AV1 = 3,
};

enum class MediaKind : std::uint8_t {
  Video = 0,
  Audio = 1,
};

const char* codec_name(CodecType c) noexcept;
CodecType codec_from_string(const std::string& s) noexcept;
const char* media_kind_name(MediaKind k) noexcept;

// Deterministic error taxonomy. errors_by_code in the report uses these names.
enum class ErrorCode : std::uint16_t {
  None = 0,
  IndexMissingEntry = 1,     // key frame index had a gap, rebuilt by scanning
  IndexOffsetOutOfRange = 2, // index offset points outside the stream
  IndexNotMonotonic = 3,     // key frame timestamps not strictly increasing
  OversizeDeclaration = 4,   // declared size exceeds the hard safety limit
  StreamTruncated = 5,       // stream ends inside a frame/NAL/OBU
  CheckpointCorrupt = 6,     // checkpoint checksum mismatch / bad format
  CheckpointUnrecoverable = 7,
  PacketUnrecoverable = 8,   // loss could not be recovered
  TimestampJump = 9,         // non-monotonic or implausible timestamp jump
  TimestampMissing = 10,     // timestamp absent, regenerated from clock
  SampleRateChanged = 11,    // audio sample rate switch, resampled
  DuplicateTimestamp = 12,   // repeated timestamp, de-duplicated
  SequenceWrap = 13,         // 16-bit sequence number wrap, handled
};

const char* error_code_name(ErrorCode c) noexcept;

struct ErrorEvent {
  ErrorCode code = ErrorCode::None;
  std::uint32_t block_id = 0; // processing block the error belongs to
  std::uint64_t sequence = 0; // packet sequence / frame index when relevant
  std::string detail;
};

// A media packet on the wire. Sequence numbers are 16-bit and wrap.
struct Packet {
  std::uint16_t sequence = 0;
  std::uint64_t linear_index = 0;  // stable position before reordering/loss
  MediaKind kind = MediaKind::Video;
  CodecType codec = CodecType::H264;
  std::int64_t pts_us = 0;      // presentation time on the media clock
  std::int64_t dts_us = 0;      // decode time on the media clock
  std::uint32_t frame_index = 0;// source frame / audio block index
  bool keyframe = false;
  std::vector<std::uint8_t> payload;
};

// One parsed video frame.
struct Frame {
  CodecType codec = CodecType::Unknown;
  std::uint32_t index = 0;
  std::int64_t pts_us = 0;
  std::int64_t dts_us = 0;
  bool keyframe = false;
  std::uint64_t offset = 0; // byte offset of the frame in the raw stream
  std::vector<std::uint8_t> data;
};

// One audio block (PCM s16, mono unless otherwise stated).
struct AudioBlock {
  std::uint32_t index = 0;
  std::int64_t pts_us = 0;
  std::uint32_t sample_rate = 48'000;
  std::uint16_t channels = 1;
  std::vector<std::int16_t> samples;
};

// Key frame index entry, persisted alongside a stream.
struct IndexEntry {
  std::uint32_t frame_index = 0;
  std::uint64_t offset = 0;
  std::int64_t pts_us = 0;
};

struct StreamIndex {
  CodecType codec = CodecType::Unknown;
  std::uint64_t declared_size = 0;
  std::vector<IndexEntry> entries;
};

// Hard safety limits: reject oversize declarations, bound memory usage.
inline constexpr std::uint64_t kMaxDeclaredStreamBytes = 512ull << 20; // 512 MiB
inline constexpr std::size_t   kMaxBufferedPackets      = 4096;
inline constexpr std::uint32_t kSequenceSpace           = 65'536;

// Signed modular distance for 16-bit sequence comparison (handles wrapping).
std::int32_t seq_distance(std::uint16_t a, std::uint16_t b) noexcept;

} // namespace media
