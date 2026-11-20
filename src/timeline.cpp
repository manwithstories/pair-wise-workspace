#include "media/timeline.hpp"

#include <cstdlib>
#include <stdexcept>

namespace mediaqc {

const char* to_string(ErrorCode code) {
    switch (code) {
        case ErrorCode::Ok: return "ok";
        case ErrorCode::Truncated: return "truncated";
        case ErrorCode::HeaderOutOfRange: return "header_out_of_range";
        case ErrorCode::ShortRead: return "short_read";
        case ErrorCode::DecodeError: return "decode_error";
        case ErrorCode::MissingIndex: return "missing_index";
        case ErrorCode::IndexGap: return "index_gap";
        case ErrorCode::KeyframeGap: return "keyframe_gap";
        case ErrorCode::BadPacketDuration: return "bad_packet_duration";
        case ErrorCode::CrcMismatch: return "crc_mismatch";
        case ErrorCode::DuplicateSegment: return "duplicate_segment";
        case ErrorCode::MissingDependency: return "missing_dependency";
        case ErrorCode::Cycle: return "cycle";
        case ErrorCode::Unrecoverable: return "unrecoverable";
    }
    return "unknown";
}

static constexpr int64_t kNanosPerSecond = 1'000'000'000LL;

SampleClock::SampleClock(int rate_hz) {
    if (rate_hz <= 0 || rate_hz > 768000)
        throw std::invalid_argument("sample rate out of range");
    rate_hz_ = rate_hz;
}

int64_t SampleClock::samples_to_ns(int64_t samples) const {
    if (rate_hz_ == 0) return 0;
    const int64_t q = samples / rate_hz_;
    const int64_t r = samples % rate_hz_;
    return q * kNanosPerSecond + r * kNanosPerSecond / rate_hz_;
}

int64_t SampleClock::map_samples(int64_t foreign_samples, int foreign_rate) const {
    if (foreign_rate <= 0)
        throw std::invalid_argument("foreign sample rate out of range");
    // Exact rational: out = in * out_rate / in_rate, remainder retained.
    const int64_t num = foreign_samples * static_cast<int64_t>(rate_hz_);
    const int64_t mapped = num / foreign_rate;
    last_residual_ns_ = std::llabs(num % foreign_rate) * kNanosPerSecond /
                        (static_cast<int64_t>(foreign_rate) * rate_hz_);
    return mapped;
}

int64_t SampleClock::period_ns() const {
    return kNanosPerSecond / rate_hz_;
}

TimelineMapper::TimelineMapper(int output_rate_hz) : clock_(output_rate_hz) {}

TimelineMapper::Mapping TimelineMapper::map_packet(int64_t source_pts_samples,
                                                   int source_rate_hz,
                                                   int source_frame_samples) {
    const int64_t src_end = source_pts_samples + source_frame_samples;
    const int64_t start = clock_.map_samples(source_pts_samples, source_rate_hz);
    const int64_t end = clock_.map_samples(src_end, source_rate_hz);
    if (clock_.last_map_residual_ns() > max_rounding_ns_)
        max_rounding_ns_ = clock_.last_map_residual_ns();
    total_samples_ = end;
    return Mapping{start, end, clock_.samples_to_ns(start), clock_.samples_to_ns(end)};
}

}  // namespace mediaqc
