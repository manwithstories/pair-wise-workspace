#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "mediacore/time.hpp"

namespace mediacore {

// Locatable, recoverable media error model. Every error carries the track
// and segment it belongs to plus an explicit time range on a known scale,
// so operators can decide the recoverable window without re-probing.
enum class ErrorCode : int {
    None = 0,
    ManifestTruncated = 10,
    ManifestInvalid,
    SegmentMissing,
    SegmentCorrupt,
    ZeroLengthRange,
    TimestampBackward,
    DuplicateSegment,
    TimelineGap,
    NonDivisibleScale,
    KeyframeIndexMiss,
    UnknownChannelLayout,
    ChannelCountMismatch,
    SampleRateConflict,
    ColorConflict,
    ColorMetadataMissing,
    DependencyCycle,
    PathEscape,
    InvalidArgument,
    IoError,
};

const char* toString(ErrorCode code);

// Errors that recovery mode may skip (the track is marked incomplete but
// the audit continues). Everything else is fatal even under --recover.
bool isRecoverable(ErrorCode code);

struct MediaError {
    ErrorCode code = ErrorCode::None;
    std::string track;
    int segment = -1;                 // segment index within the track
    std::string uri;
    TimeRange range;                  // located window, base may be {0,1}
    bool recoverable = true;
    std::string detail;
};

class ErrorLog {
public:
    void add(MediaError err);
    const std::vector<MediaError>& items() const { return items_; }
    std::size_t size() const { return items_.size(); }
    std::size_t count(ErrorCode code) const;
    bool hasFatal() const;            // any non-recoverable item
    std::vector<MediaError> forTrack(const std::string& track) const;

private:
    std::vector<MediaError> items_;
};

}  // namespace mediacore
