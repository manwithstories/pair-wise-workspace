#pragma once

#include "framebuffer.hpp"
#include "hash.hpp"
#include "time_map.hpp"

#include <array>
#include <cstdint>
#include <string>
#include <vector>

namespace bake {

struct ClipSpec {
    std::string id;
    std::string directory;
    int64_t frames = 0;
    int64_t start = 0;
};

struct TransitionSpec {
    int from = 0;
    int to = 0;
    std::string kernel;
    int64_t start = 0;
    int64_t frames = 0;
    int priority = 0;
};

struct LoadedClip {
    ClipSpec spec;
    std::vector<FrameBuffer> frames;
    std::vector<std::string> frame_errors;
};

struct ClassEvidence {
    std::string class_name;
    int frame_count = 0;
    uint64_t first_frame_hash = 0;
    uint64_t last_frame_hash = 0;
    uint32_t mean_abs_diff = 0;
    uint32_t max_abs_diff = 0;
    uint32_t changed_pixels = 0;
    std::array<uint64_t, kDiffHistogramBins> diff_histogram{};
    int diff_peak_frame = 0;
    std::vector<uint32_t> diff_curve;
    std::string curve_kind;
};

class Timeline {
public:
    static Timeline load(const std::string& path);

    int width = 0;
    int height = 0;
    uint64_t seed = 0;
    std::vector<ClipSpec> clips;
    std::vector<TransitionSpec> transitions;
    std::vector<SpeedInterval> speed;
    std::vector<TimeMap> time_maps;

    int64_t totalFrames() const;
    const TransitionSpec* transitionAt(int64_t frame) const;
    bool isIntentionalHold(int64_t frame) const;
    std::vector<LoadedClip> loadSources(const std::string& source_root,
                                        bool continue_on_error = false) const;
    void renderFrame(int64_t frame, const std::vector<LoadedClip>& sources,
                     FrameBuffer& destination) const;

private:
    void validate();
    TimePoint mapClipFrame(std::size_t clip_index, int64_t frame) const;
};

std::string frameFileName(int64_t frame);
std::vector<ClassEvidence> computeClassEvidence(int width, int height, uint64_t seed);

}  // namespace bake
