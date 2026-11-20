#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/time.hpp"

namespace mediacore {

enum class MediaKind { Unknown, Video, Audio, Subtitle, Data };
const char* toString(MediaKind kind);

struct StreamInfo {
    int index = -1;
    MediaKind kind = MediaKind::Unknown;
    std::string codecName;
    std::string codecLongName;
    std::string profileName;
    int64_t bitrate = 0;
    // Video
    int width = 0;
    int height = 0;
    Rational frameRate{0, 1};
    Rational sampleAspect{0, 1};
    // Audio
    int sampleRate = 0;
    int channels = 0;
    std::string channelLayout;  // empty when unknown
    // Timing
    Rational timeBase{1, 1};
    Tick firstPts = 0;
    Tick lastPts = 0;
    Tick durationTicks = 0;
    // Color metadata; "unknown" sentinels map to ColorMetadata::unknown.
    int colorRange = 0;        // AVColorRange
    int colorPrimaries = 0;    // AVColorPrimaries
    int colorTransfer = 0;     // AVColorTransferCharacteristic
    int colorSpace = 0;        // AVColorSpace (matrix)
};

struct KeyframePoint {
    Tick pts = 0;
    Rational base{1, 1};
    std::size_t packetOrdinal = 0;
};

struct KeyframeGap {
    TimeRange range;
    Tick expectedInterval = 0;
    Tick actualInterval = 0;
};

struct SegmentProbe {
    std::string uri;
    bool opened = false;
    bool tailCorrupt = false;
    std::string containerFormat;
    std::vector<StreamInfo> streams;
    std::vector<KeyframePoint> keyframes;
    std::vector<KeyframeGap> keyframeGaps;
    Rational keyframeBase{1, 1};
    int64_t fileBytes = 0;
    int64_t packetCount = 0;
    // Timestamp anomalies discovered while scanning packets.
    bool backwardTimestamps = false;
    bool duplicateTimestamps = false;
};

struct ProbeOptions {
    // Stop scanning a segment after this many packets (per-track memory
    // bound); zero means unlimited.
    std::size_t maxPacketsPerSegment = 200'000;
    // Expected keyframe interval in the stream's time-base ticks. Zero
    // disables index-hit evaluation.
    Tick expectedKeyframeInterval = 0;
    // Declared segment duration in seconds. When the packets actually
    // readable cover less than this fraction, the tail is treated as
    // damaged (demuxers otherwise surface truncation as plain EOF).
    Rational declaredDuration{0, 1};
    double minDurationCoverage = 0.85;
    // Absolute timeline start tick (on the probe's time base) used to
    // evaluate keyframe index hits for standalone per-file segments whose
    // own PTS restarts at zero.
    Tick expectedStartTick = 0;
};

// Probe one media segment. Corrupt containers produce a located
// SegmentCorrupt error and a partially populated result rather than
// throwing, so recovery mode can skip the window.
SegmentProbe probeSegment(const std::filesystem::path& path,
                          const std::string& track, int segmentIndex,
                          const ProbeOptions& opts, ErrorLog& errors);

// Evaluate whether probed keyframes match the manifest-declared segment
// boundary index. A hit means the first keyframe of the segment lands on
// the expected boundary tick. Returns the per-segment hit ratio.
struct IndexHitSummary {
    std::size_t evaluated = 0;
    std::size_t hits = 0;
    double hitRate() const {
        return evaluated == 0 ? 1.0
                              : static_cast<double>(hits) / evaluated;
    }
};

IndexHitSummary evaluateKeyframeIndex(
    const std::vector<SegmentProbe>& probes,
    const std::vector<TimeRange>& expectedBoundaries);

// Flag keyframe spacing that deviates from the median interval.
std::vector<KeyframeGap> analyzeKeyframeGaps(const SegmentProbe& probe,
                                             double toleranceRatio = 0.5);

}  // namespace mediacore
