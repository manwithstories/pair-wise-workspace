#pragma once

#include <string>
#include <vector>

#include "mediacore/error.hpp"

extern "C" {
#include <libavutil/samplefmt.h>
}

namespace mediacore {

// Observed or requested audio specification. Layout is carried as the
// FFmpeg channel-layout description string ("stereo", "5.1(side)", ...);
// an empty/unknown layout is never silently mapped to something else.
struct AudioSpec {
    int sampleRate = 0;
    int channels = 0;
    std::string layout;
    AVSampleFormat sampleFormat = AV_SAMPLE_FMT_NONE;

    bool valid() const {
        return sampleRate > 0 && channels > 0 && !layout.empty();
    }
    std::string key() const;
};

enum class AudioAction {
    None,            // already conforms
    ResampleRate,    // sample-rate conversion only
    Remix,           // channel layout / count conversion
    ConvertFormat,   // sample-format conversion
    Rejected,        // cannot plan safely
};

const char* toString(AudioAction action);

struct ResampleStep {
    AudioAction action = AudioAction::None;
    std::string detail;
};

struct ResamplePlan {
    std::string track;
    AudioSpec source;
    AudioSpec target;
    bool feasible = false;
    bool layoutVerified = false;
    std::vector<ResampleStep> steps;
    // Estimated swresample buffering in destination samples; zero when the
    // conversion needs no resampling delay.
    int64_t estimatedDelaySamples = 0;
};

struct AudioTarget {
    int sampleRate = 48000;
    int channels = 2;
    std::string layout = "stereo";
    AVSampleFormat sampleFormat = AV_SAMPLE_FMT_FLTP;
};

// Verify that a spec's channel layout can be resolved by libavutil and
// that its reported channel count agrees with the layout. Unknown layouts
// and count mismatches are retained as errors rather than guessed.
bool verifyChannelLayout(const std::string& track, int segmentIndex,
                         const AudioSpec& spec, ErrorLog& errors);

// Build an explicit, swresample-validated conversion plan. The plan never
// invents a channel mapping: an unresolvable source layout yields a
// Rejected plan and a located error.
ResamplePlan planAudioNormalization(const std::string& track,
                                    int segmentIndex,
                                    const AudioSpec& source,
                                    const AudioTarget& target,
                                    ErrorLog& errors);

// Aggregate per-segment specs and flag intra-track conflicts (different
// sample rates or channel counts across segments of the same track).
std::vector<ResampleStep> detectAudioConflicts(
    const std::string& track, const std::vector<AudioSpec>& specs,
    ErrorLog& errors);

}  // namespace mediacore
