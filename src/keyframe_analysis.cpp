#include <algorithm>

#include "mediacore/probe.hpp"

namespace mediacore {

// Compute keyframe spacing within each probed segment and flag gaps that
// deviate from the median interval by more than a tolerance. Spacing is
// measured in the segment's own time base, so no rounding is introduced.
std::vector<KeyframeGap> analyzeKeyframeGaps(const SegmentProbe& probe,
                                             double toleranceRatio) {
    std::vector<KeyframeGap> gaps;
    if (probe.keyframes.size() < 2) return gaps;
    std::vector<Tick> deltas;
    deltas.reserve(probe.keyframes.size() - 1);
    for (std::size_t i = 1; i < probe.keyframes.size(); ++i) {
        const Tick d = probe.keyframes[i].pts -
                       probe.keyframes[i - 1].pts;
        if (d > 0) deltas.push_back(d);
    }
    if (deltas.empty()) return gaps;
    std::vector<Tick> sorted = deltas;
    std::sort(sorted.begin(), sorted.end());
    const Tick median = sorted[sorted.size() / 2];

    for (std::size_t i = 1; i < probe.keyframes.size(); ++i) {
        const Tick d = probe.keyframes[i].pts -
                       probe.keyframes[i - 1].pts;
        if (d <= 0) continue;
        const double ratio =
            median == 0 ? 1.0
                        : static_cast<double>(d) /
                              static_cast<double>(median);
        if (ratio > 1.0 + toleranceRatio ||
            ratio < 1.0 - toleranceRatio) {
            TimeRange r{probe.keyframes[i - 1].pts,
                        probe.keyframes[i].pts, probe.keyframeBase};
            gaps.push_back(KeyframeGap{r, median, d});
        }
    }
    return gaps;
}

}  // namespace mediacore
