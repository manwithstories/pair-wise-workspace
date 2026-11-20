#include "mediacore/error.hpp"

namespace mediacore {

const char* toString(ErrorCode code) {
    switch (code) {
        case ErrorCode::None: return "none";
        case ErrorCode::ManifestTruncated: return "manifest_truncated";
        case ErrorCode::ManifestInvalid: return "manifest_invalid";
        case ErrorCode::SegmentMissing: return "segment_missing";
        case ErrorCode::SegmentCorrupt: return "segment_corrupt";
        case ErrorCode::ZeroLengthRange: return "zero_length_range";
        case ErrorCode::TimestampBackward: return "timestamp_backward";
        case ErrorCode::DuplicateSegment: return "duplicate_segment";
        case ErrorCode::TimelineGap: return "timeline_gap";
        case ErrorCode::NonDivisibleScale: return "non_divisible_scale";
        case ErrorCode::KeyframeIndexMiss: return "keyframe_index_miss";
        case ErrorCode::UnknownChannelLayout:
            return "unknown_channel_layout";
        case ErrorCode::ChannelCountMismatch:
            return "channel_count_mismatch";
        case ErrorCode::SampleRateConflict: return "sample_rate_conflict";
        case ErrorCode::ColorConflict: return "color_conflict";
        case ErrorCode::ColorMetadataMissing:
            return "color_metadata_missing";
        case ErrorCode::DependencyCycle: return "dependency_cycle";
        case ErrorCode::PathEscape: return "path_escape";
        case ErrorCode::InvalidArgument: return "invalid_argument";
        case ErrorCode::IoError: return "io_error";
    }
    return "unknown";
}

bool isRecoverable(ErrorCode code) {
    switch (code) {
        case ErrorCode::SegmentMissing:
        case ErrorCode::SegmentCorrupt:
        case ErrorCode::ZeroLengthRange:
        case ErrorCode::KeyframeIndexMiss:
        case ErrorCode::TimestampBackward:
        case ErrorCode::DuplicateSegment:
        case ErrorCode::TimelineGap:
        case ErrorCode::NonDivisibleScale:
            return true;
        default:
            return false;
    }
}

void ErrorLog::add(MediaError err) {
    if (err.code == ErrorCode::None) return;
    if (err.detail.empty()) err.detail = toString(err.code);
    items_.push_back(std::move(err));
}

std::size_t ErrorLog::count(ErrorCode code) const {
    std::size_t n = 0;
    for (const auto& e : items_)
        if (e.code == code) ++n;
    return n;
}

bool ErrorLog::hasFatal() const {
    for (const auto& e : items_)
        if (!e.recoverable) return true;
    return false;
}

std::vector<MediaError> ErrorLog::forTrack(
    const std::string& track) const {
    std::vector<MediaError> out;
    for (const auto& e : items_)
        if (e.track == track) out.push_back(e);
    return out;
}

}  // namespace mediacore
