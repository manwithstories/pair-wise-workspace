#pragma once

#include <array>
#include <cstdint>
#include <limits>
#include <map>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace mediaqc {

// The one clock every module shares: signed nanoseconds since the track
// epoch.  Values are always integral; no floating point enters the
// timeline so repaired timestamps are reproducible bit for bit.
using Ns = std::int64_t;

inline constexpr Ns kNoTimestamp = std::numeric_limits<Ns>::min();

inline constexpr Ns kNsPerSecond = 1000000000LL;

struct Rational {
  std::int32_t num = 0;
  std::int32_t den = 1;

  [[nodiscard]] bool valid() const { return num > 0 && den > 0; }
  friend bool operator==(const Rational&, const Rational&) = default;
};

// A device clock counter.  `bits == 0` means the counter never wraps and
// values are already ticks at `hz` (or nanoseconds when hz == 1e9).
struct ClockSpec {
  std::uint32_t hz = 1000000000u;
  std::uint8_t bits = 0;
  std::uint64_t epoch_ticks = 0;  // counter value that defines timeline zero
  bool has_epoch = false;
};

// Logical track kinds we accept.  Anything else is preserved as data.
enum class TrackKind { Video, Audio, Data };

[[nodiscard]] std::string_view to_string(TrackKind kind);
[[nodiscard]] std::optional<TrackKind> parse_track_kind(std::string_view text);

// Pixel formats are stored as a small stable id so the container never
// depends on the host FFmpeg enum ordering.
enum class PixelFormatId : std::uint32_t {
  kUnknown = 0,
  kYuv420p = 1,
  kYuv422p = 2,
  kYuv444p = 3,
  kGray = 4,
  kRgb24 = 5,
};

[[nodiscard]] std::string_view to_string(PixelFormatId id);
[[nodiscard]] std::optional<PixelFormatId> parse_pixel_format(std::string_view);

// Everything the pipeline needs to know about one track before a frame is
// read: decoding parameters, timing, dependency edges and the metadata
// schema used to validate per-frame attributes.
struct TrackDesc {
  std::string id;
  TrackKind kind = TrackKind::Video;
  std::string codec;      // e.g. "ffv1", "pcm_s16le"
  std::string pix_fmt;    // video only, e.g. "yuv420p"
  int width = 0;
  int height = 0;
  int sample_rate = 0;
  int channels = 0;
  Rational rate;          // frames per second for video
  ClockSpec clock;
  std::vector<std::string> depends_on;
  std::vector<std::string> required_fields;
  std::vector<std::string> optional_fields;
  std::vector<std::uint8_t> extradata;  // codec extradata, if any
};

// One frame record exactly as framed by the input container.  `offset`
// is the absolute byte position of the record length field, which is what
// every corruption report points at.
struct FrameRecord {
  std::uint64_t index = 0;          // 0-based order in the file
  std::uint64_t offset = 0;         // record-start byte offset
  std::size_t track_index = 0;
  std::uint64_t seq = 0;
  std::int64_t capture_ticks = 0;   // raw device counter
  bool has_capture_time = false;
  bool keyframe = false;
  PixelFormatId pix_fmt = PixelFormatId::kUnknown;
  int width = 0;
  int height = 0;
  std::array<std::uint8_t, 32> sha256{};
  std::string attrs_json;           // per-frame shooting metadata object
  std::vector<std::uint8_t> payload;
};

// Why a timestamp differs from what the supplier wrote.
enum class RepairReason : std::uint32_t {
  kNone = 0,
  kMissing = 1,        // no captured time; derived from the frame rate
  kNonMonotonic = 2,   // clock went backwards or repeated
  kWraparound = 4,     // device counter rolled over
  kDrift = 8,          // clock rate disagrees with the configured frame rate
};

using RepairMask = std::uint32_t;

[[nodiscard]] RepairMask operator|(RepairMask a, RepairReason b);
[[nodiscard]] bool has_reason(RepairMask mask, RepairReason reason);
[[nodiscard]] std::vector<std::string> repair_reason_names(RepairMask mask);

// A single timestamp that the timeline changed, kept for the manifest
// and for machine-readable debugging.  Every repaired frame produces one.
struct RepairRecord {
  std::string track;
  std::uint64_t seq = 0;
  std::uint64_t record_offset = 0;
  Ns raw_ns = 0;
  Ns fixed_ns = 0;
  RepairMask reasons = 0;
};

// Machine-readable failure categories map onto the process exit codes
// promised by the command line contract.
enum class ErrorCategory {
  kInvalidArgument,  // exit 2
  kInvalidInput,     // exit 2
  kCorruption,       // exit 3
  kIo,               // exit 4
  kRecovery,         // exit 4
};

[[nodiscard]] int exit_code_for(ErrorCategory category);

struct Error {
  ErrorCategory category = ErrorCategory::kInvalidInput;
  std::string code;                 // stable slug, e.g. "checksum_mismatch"
  std::string message;              // human readable
  std::string track;                // empty when not track-specific
  std::uint64_t offset = 0;         // record-start byte offset
  bool has_offset = false;
  std::uint64_t seq = 0;
  bool has_seq = false;

  [[nodiscard]] int exit_code() const { return exit_code_for(category); }
};

}  // namespace mediaqc
