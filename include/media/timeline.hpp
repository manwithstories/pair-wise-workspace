#pragma once
// Unified sample clock, segment boundaries and the shared error model.
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace mediaqc {

// Errors every module records into the same model, each carrying a source
// byte offset so damage can be located on the original file.
enum class ErrorCode {
    Ok,
    Truncated,            // file ended inside a sample frame
    HeaderOutOfRange,     // RIFF/WAV header field inconsistent or implausible
    ShortRead,            // fewer bytes on disk than the header declares
    DecodeError,          // codec rejected a packet
    MissingIndex,         // index page absent or unreadable
    IndexGap,             // index pages are not contiguous
    KeyframeGap,          // packet spacing exceeds the configured threshold
    BadPacketDuration,    // packet samples do not match its duration
    CrcMismatch,          // indexed page checksum does not match content
    DuplicateSegment,     // two blocks cover the same timeline range
    MissingDependency,    // track graph references an unknown block
    Cycle,                // track graph contains a dependency cycle
    Unrecoverable,        // no decodable boundary exists
};

const char* to_string(ErrorCode code);

struct DamageRecord {
    ErrorCode code = ErrorCode::Ok;
    int64_t source_offset = -1;   // byte offset in the source file, -1 if unknown
    int64_t start_sample = 0;     // first affected sample on the unified clock
    int64_t end_sample = 0;       // one past the last affected sample
    std::string detail;
};

// Clock domain: every time is kept as an integer sample position and mapped
// to nanoseconds through an exact rational scale. No floating-point time math.
class SampleClock {
public:
    SampleClock() = default;
    explicit SampleClock(int rate_hz);

    int rate_hz() const { return rate_hz_; }

    // Exact conversion sample position (in this clock) -> nanoseconds, rounded
    // toward zero; the remainder is exposed so callers can bound the error.
    int64_t samples_to_ns(int64_t samples) const;
    // Map a sample position in a foreign rate onto this clock exactly.
    int64_t map_samples(int64_t foreign_samples, int foreign_rate) const;
    // Residual in nanoseconds of the last map_samples call (< one output period).
    int64_t last_map_residual_ns() const { return last_residual_ns_; }
    int64_t period_ns() const;  // one output sample period in nanoseconds

private:
    int rate_hz_ = 48000;
    mutable int64_t last_residual_ns_ = 0;
};

// A contiguous half-open [start_sample, end_sample) range on a clock.
struct Segment {
    int64_t start_sample = 0;
    int64_t end_sample = 0;
    bool overlaps(const Segment& o) const {
        return start_sample < o.end_sample && o.start_sample < end_sample;
    }
    int64_t length() const { return end_sample - start_sample; }
};

// Streamed packet timestamp mapped onto the unified clock. The mapper keeps an
// exact running remainder so accumulated conversion error stays below one
// output sample period.
class TimelineMapper {
public:
    explicit TimelineMapper(int output_rate_hz);

    // Map one packet given its source sample position and length.
    struct Mapping {
        int64_t start_sample;
        int64_t end_sample;
        int64_t start_ns;
        int64_t end_ns;
    };
    Mapping map_packet(int64_t source_pts_samples, int source_rate_hz, int source_frame_samples);

    const SampleClock& clock() const { return clock_; }
    int64_t total_samples() const { return total_samples_; }
    int64_t max_rounding_ns() const { return max_rounding_ns_; }

private:
    SampleClock clock_;
    int64_t total_samples_ = 0;
    int64_t max_rounding_ns_ = 0;
};

}  // namespace mediaqc
