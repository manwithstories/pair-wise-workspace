#pragma once

// Shared data model for the media frame repair pipeline.
//
// A capture stream is stored in the MFS1 container: a fixed file header
// followed by length-prefixed packet records. Every record carries versioned
// metadata, a raw capture clock, the codec payload and a SHA-256 checksum.
// All integers on disk are big-endian.

#include <array>
#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <string>
#include <vector>

namespace mfs {

inline constexpr uint32_t kFileMagic = 0x4D465331u;    // "MFS1"
inline constexpr uint32_t kPacketMagic = 0x4D465031u;  // "MFP1"
inline constexpr uint16_t kFormatVersion = 1;
inline constexpr uint16_t kMetadataVersion = 1;
inline constexpr size_t kFileHeaderSize = 32;
inline constexpr size_t kSha256Size = 32;
inline constexpr size_t kMaxMetadataFields = 64;

inline constexpr uint16_t kFlagKeyframe = 1u << 0;
inline constexpr uint16_t kFlagClockMissing = 1u << 1;
inline constexpr uint16_t kKnownMetaFlags = kFlagKeyframe | kFlagClockMissing;

// Known metadata field identifiers. Unknown *required* fields cause a packet
// to be rejected; unknown *optional* fields are preserved byte-for-byte.
inline constexpr uint16_t kFieldCameraId = 0x0001;
inline constexpr uint16_t kFieldLinkType = 0x0002;
inline constexpr uint16_t kFieldCaptureNote = 0x0003;
// Arrival timestamp (microseconds, big-endian u64) recorded by the capture
// side. Used to adapt the reorder window to time-varying link bandwidth.
inline constexpr uint16_t kFieldArrivalUs = 0x0004;

// Default resource guards (also overridable on the command line).
inline constexpr size_t kDefaultMaxFrameBytes = 8u * 1024u * 1024u;
inline constexpr size_t kDefaultMaxTotalBytes = 256u * 1024u * 1024u;
inline constexpr uint32_t kDefaultMaxSegments = 64;
inline constexpr uint32_t kDefaultReorderWindow = 16;
inline constexpr double kDefaultDuplicateRate = 0.08;
inline constexpr uint32_t kDefaultSegmentFrames = 120;

enum class CodecId : uint8_t { Png = 1, Mjpeg = 2 };
enum class FieldKind : uint8_t { Required = 0, Optional = 1 };

const char* to_string(CodecId codec);

// Time base used by repaired frames and by the random-access index.
struct Rational {
  int32_t num{1};
  int32_t den{1000000};

  constexpr bool valid() const { return num > 0 && den > 0; }
  friend constexpr bool operator==(const Rational&, const Rational&) = default;
};

inline constexpr Rational kMicrosecondBase{1, 1000000};
inline constexpr Rational kStreamTimeBase{1, 90000};

struct MetadataField {
  uint16_t id{0};
  FieldKind kind{FieldKind::Optional};
  std::vector<uint8_t> value;

  friend bool operator==(const MetadataField&, const MetadataField&) = default;
};

struct FileHeader {
  uint16_t version{kFormatVersion};
  uint32_t flags{0};
  uint64_t stream_id{0};
  uint32_t nominal_hz{1000000};
};

struct PacketHeader {
  uint16_t meta_version{kMetadataVersion};
  uint16_t meta_flags{0};
  uint64_t frame_seq{0};
  uint32_t capture_clock{0};   // raw capture ticks; 0 means "missing"
  uint32_t capture_hz{1000000};
  uint32_t width{0};
  uint32_t height{0};
  CodecId codec{CodecId::Png};
  std::vector<MetadataField> metadata;

  bool keyframe() const { return (meta_flags & kFlagKeyframe) != 0; }
  // The capture clock is missing when the sender says so, or when a zero
  // clock value is present (zero is reserved as "no timestamp").
  bool clock_missing() const {
    return (meta_flags & kFlagClockMissing) != 0 || capture_clock == 0;
  }
};

struct Packet {
  PacketHeader header;
  std::vector<uint8_t> payload;
  std::array<uint8_t, kSha256Size> checksum{};
  uint64_t raw_offset{0};     // byte offset of the record magic in the file
  uint32_t record_length{0};  // body length following the length field
  std::string decode_detail;  // parser diagnostic for rejected records

  uint64_t total_size() const { return 8u + record_length; }
};

// A received frame after sequence reordering and timestamp repair. Decode
// fields are filled by the FFmpeg transform stage.
struct RepairedFrame {
  uint64_t seq{0};
  int64_t ts_us{0};             // repaired, strictly increasing timeline
  int64_t original_ts_us{-1};   // unwrapped original clock, -1 when missing
  uint32_t raw_clock{0};
  uint32_t capture_hz{1000000};
  bool clock_missing{false};
  bool keyframe{false};
  uint32_t width{0};
  uint32_t height{0};
  CodecId codec{CodecId::Png};
  uint64_t source_offset{0};
  std::array<uint8_t, kSha256Size> payload_sha256{};
  int64_t pts{0};               // timestamp converted to time_base
  Rational time_base{kStreamTimeBase};
  bool decoded{false};
  uint32_t decoded_width{0};
  uint32_t decoded_height{0};
  std::array<uint8_t, kSha256Size> frame_sha256{};  // canonical RGB content
};

struct IndexEntry {
  uint32_t segment_id{0};
  uint64_t keyframe_seq{0};
  int64_t start_ts_us{0};
  int64_t end_ts_us{0};  // exclusive
  int64_t pts{0};        // keyframe timestamp in time_base
  Rational time_base{kStreamTimeBase};
  uint64_t source_offset{0};
  uint32_t frame_count{0};
  std::array<uint8_t, kSha256Size> segment_sha256{};
};

struct LostRange {
  uint64_t first_seq{0};
  uint64_t last_seq{0};
  int64_t estimated_start_us{0};
  int64_t estimated_end_us{0};
  uint32_t estimated_frames{0};
};

// A run of sequence numbers that will never be delivered. Produced by the
// reorder stage; timestamp estimates are attached later.
struct SequenceGap {
  uint64_t first_seq{0};
  uint64_t last_seq{0};
};

enum class ErrorType {
  FileHeaderTruncated,
  FileHeaderInvalid,
  UnsupportedFileVersion,
  RecordTruncated,
  RecordMagicMismatch,
  LengthOverflow,
  ChecksumMismatch,
  UnknownRequiredField,
  UnsupportedMetadata,
  OutOfWindow,
  DecodeError,
  LimitExceeded,
  EmptySequence,
  RoundtripMismatch,
  SegmentLimit,
};

const char* to_string(ErrorType type);

struct RepairError {
  ErrorType type{ErrorType::LimitExceeded};
  uint64_t offset{0};
  uint32_t record_index{0};
  std::string detail;
};

struct TimestampStats {
  uint64_t wrap_fixes{0};
  uint64_t duplicate_timestamps{0};
  uint64_t backward_timestamps{0};
  uint64_t missing_timestamps{0};
  uint64_t drift_fixes{0};
  int64_t nominal_interval_us{0};
  bool strictly_increasing{true};
};

struct RepairResult {
  uint64_t input_bytes{0};
  uint64_t records_read{0};
  uint64_t packets_read{0};
  uint64_t packets_accepted{0};
  uint64_t repaired_frames{0};
  uint64_t duplicate_packets{0};
  uint64_t lost_ranges{0};
  uint64_t decode_errors{0};
  int64_t first_error_offset{-1};

  double roundtrip_match_rate{0.0};
  double keyframe_index_hit_rate{0.0};
  double segment_hash_match_rate{0.0};
  uint64_t roundtrip_checked{0};
  uint64_t roundtrip_mismatches{0};
  uint64_t index_queries{0};
  uint64_t index_hits{0};
  uint64_t segments_hashed{0};
  uint64_t segment_hash_matches{0};

  bool truncated_input{false};
  TimestampStats timestamps;
  std::vector<RepairError> errors;
  std::vector<LostRange> lost;
  std::vector<IndexEntry> index;
  std::vector<RepairedFrame> frames;

  // True when at least one structural problem made the stream unfixable.
  bool empty_sequence{false};
  bool limit_hit{false};
  uint64_t out_of_window_packets{0};
  uint64_t checksum_errors{0};
  uint64_t truncated_records{0};
};

// Unified error type. A MediaError with a file-format type maps to exit code 2
// at the CLI boundary; anything else is treated as an internal failure (3).
class MediaError : public std::runtime_error {
 public:
  MediaError(ErrorType type, std::string message, uint64_t offset = 0)
      : std::runtime_error(std::move(message)), type_(type), offset_(offset) {}

  ErrorType type() const noexcept { return type_; }
  uint64_t offset() const noexcept { return offset_; }

 private:
  ErrorType type_;
  uint64_t offset_;
};

}  // namespace mfs
