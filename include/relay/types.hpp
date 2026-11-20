#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace relay {

/// All media times use one explicit time base: microseconds since the
/// unwrapped capture epoch.  Raw capture timestamps are 32-bit microsecond
/// counters and are unwrapped into this type.
using Micros = std::int64_t;
using TrackId = std::string;

inline constexpr std::size_t kMaxTrackCacheBytes = 256ull * 1024ull * 1024ull;
inline constexpr std::int64_t kTimestampModulus = std::int64_t{1} << 32;
inline constexpr std::int32_t kLongFrameDurationUs = 33333;

/// Exit codes are part of the CLI contract.
enum class ExitCode : int {
  kOk = 0,
  kUsage = 2,
  kCorrupt = 3,
};

enum class Severity { kWarning, kError };

struct Ids {
  std::string stage;
  std::string track;
  std::uint64_t frame = 0;
};

/// One structured diagnostic.  Every data-path failure carries enough
/// context to locate the camera (track) and source frame number.
struct RelayError {
  Severity severity = Severity::kError;
  ExitCode exit_code = ExitCode::kCorrupt;
  Ids ids;
  std::string reason;
  std::string dependency_chain;

  [[nodiscard]] std::string to_string() const;
};

class ErrorSink {
 public:
  void warn(std::string stage, std::string track, std::uint64_t frame,
            std::string reason);
  void fail(std::string stage, std::string track, std::uint64_t frame,
            std::string reason, std::string dependency_chain = {});
  void add(RelayError error);

  [[nodiscard]] bool has_errors() const;
  [[nodiscard]] bool has_warnings() const;
  [[nodiscard]] int exit_code() const;
  [[nodiscard]] const std::vector<RelayError>& events() const { return events_; }
  [[nodiscard]] std::size_t count(Severity severity) const;

 private:
  std::vector<RelayError> events_;
};

struct ExposureMetadata {
  int iso = 0;
  int shutter_us = 0;
  double aperture = 0.0;
};

/// Metadata for one verified image frame.  Payload bytes themselves are
/// released as soon as SHA-256 verification and FFmpeg decoding finish.
struct FrameRecord {
  TrackId track;
  std::uint64_t source_frame = 0;
  std::uint32_t sequence = 0;
  std::uint32_t raw_timestamp_us = 0;
  Micros pts_us = 0;
  int epoch = 0;
  std::string pixel_format;
  int width = 0;
  int height = 0;
  ExposureMetadata exposure;
  bool keyframe = false;
  std::string sha256;
  std::size_t payload_bytes = 0;
};

/// A frame as it arrived on the capture link (possibly late or duplicated).
struct Packet {
  FrameRecord frame;
  std::uint64_t arrival_index = 0;
};

struct LostRange {
  TrackId track;
  int epoch = 0;
  std::uint32_t first_sequence = 0;
  std::uint32_t count = 0;
  std::string cause;
};

struct EpochInfo {
  int index = 0;
  std::string cause;
  Micros start_us = 0;
  Micros end_us = 0;
  std::uint32_t first_sequence = 0;
  std::uint32_t last_sequence = 0;
};

struct Timebase {
  int fps_num = 30;
  int fps_den = 1;

  [[nodiscard]] double frame_duration_us() const {
    return 1'000'000.0 * fps_den / fps_num;
  }
  [[nodiscard]] Micros frames_to_us(std::int64_t frames) const;
  [[nodiscard]] std::int64_t us_to_frames(Micros us) const;
};

enum class LossMode { kBurst, kSingleFrame };

struct TrackPolicy {
  LossMode loss_mode = LossMode::kBurst;
  std::uint32_t reorder_window = 32;
  std::size_t cache_limit_bytes = kMaxTrackCacheBytes;
};

struct TrackSpec {
  TrackId id;
  std::string kind = "video";
  std::vector<TrackId> depends_on;
  TrackPolicy policy;
  bool required = true;
};

}  // namespace relay
