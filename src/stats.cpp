#include "stats.h"

namespace h264 {

StatsSummary compute_stats(const std::vector<AccessUnit>& aus,
                           const std::vector<AccessUnit>& frames,
                           const std::vector<GopInfo>& gops,
                           const std::vector<Warning>& warnings) {
    StatsSummary stats;
    stats.total_frames = frames.size();
    for (const AccessUnit& frame : frames) {
        switch (frame.slice_type) {
            case 2:
            case 4: ++stats.i_frames; break;
            case 0:
            case 3: ++stats.p_frames; break;
            case 1: ++stats.b_frames; break;
            default: ++stats.other_frames; break;
        }
    }
    for (const AccessUnit& au : aus) {
        ++stats.slices_histogram[au.slice_count];
        stats.total_slices += au.slice_count;
        if (au.slice_count > stats.slices_per_au_max) {
            stats.slices_per_au_max = au.slice_count;
        }
    }
    std::uint64_t gop_bytes = 0;
    for (const GopInfo& gop : gops) {
        gop_bytes += gop.byte_length;
        stats.total_bytes += gop.byte_length;
    }
    if (!gops.empty()) {
        stats.average_gop_bytes = static_cast<double>(gop_bytes) /
                                  static_cast<double>(gops.size());
    }
    for (const Warning& warning : warnings) {
        ++stats.warning_counts[warning.level];
    }
    return stats;
}

}  // namespace h264
