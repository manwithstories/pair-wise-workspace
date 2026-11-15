#include "gop_index.h"

#include <algorithm>
#include <sstream>

namespace h264 {
namespace {

bool is_boundary(const AccessUnit& frame, bool use_recovery_points) {
    return frame.idr || (use_recovery_points && frame.recovery_point);
}

void fill_gop(const std::vector<NalUnit>& nals,
              const std::vector<AccessUnit>& frames,
              std::uint64_t start_offset,
              std::uint64_t end_offset,
              std::size_t first_frame,
              std::size_t last_frame,
              std::size_t index,
              GopInfo& gop) {
    gop = GopInfo{};
    gop.index = index;
    gop.start_offset = start_offset;
    gop.end_offset = end_offset;
    gop.byte_length = end_offset >= start_offset ? end_offset - start_offset : 0;
    gop.first_nal = frames[first_frame].first_nal;
    gop.last_nal = frames[last_frame].last_nal;
    gop.frame_count = last_frame - first_frame + 1;
    gop.starts_with_recovery_point = frames[first_frame].recovery_point && !frames[first_frame].idr;
    for (std::size_t i = first_frame; i <= last_frame; ++i) {
        gop.frame_indices.push_back(i);
    }
    std::stable_sort(gop.frame_indices.begin(), gop.frame_indices.end(),
                     [&](std::size_t a, std::size_t b) {
                         if (frames[a].poc != frames[b].poc) {
                             return frames[a].poc < frames[b].poc;
                         }
                         return frames[a].start_offset < frames[b].start_offset;
                     });
    for (const std::size_t frame_index : gop.frame_indices) {
        const int type = frames[frame_index].slice_type;
        gop.frame_types.push_back(slice_type_name(type)[0]);
    }
    (void)nals;
}

}  // namespace

bool build_gops(const std::vector<NalUnit>& nals,
                const std::vector<AccessUnit>& frames,
                std::uint64_t file_size,
                bool use_recovery_points,
                std::size_t min_gop_frames,
                std::vector<GopInfo>& gops,
                std::string& error) {
    (void)error;
    gops.clear();
    if (frames.empty()) {
        return false;
    }
    std::vector<std::size_t> boundaries;
    boundaries.push_back(0);
    for (std::size_t i = 1; i < frames.size(); ++i) {
        if (!is_boundary(frames[i], use_recovery_points)) {
            continue;
        }
        if (!frames[i].idr) {
            const std::size_t distance = i - boundaries.back();
            if (distance < min_gop_frames) {
                continue;
            }
        }
        boundaries.push_back(i);
    }
    for (std::size_t i = 0; i < boundaries.size(); ++i) {
        const std::size_t first_frame = boundaries[i];
        const std::size_t last_frame = i + 1 < boundaries.size()
                                           ? boundaries[i + 1] - 1
                                           : frames.size() - 1;
        const std::uint64_t start_offset = frames[first_frame].start_offset;
        const std::uint64_t end_offset = i + 1 < boundaries.size()
                                             ? frames[last_frame + 1].start_offset
                                             : file_size;
        GopInfo gop;
        fill_gop(nals, frames, start_offset, end_offset,
                 first_frame, last_frame, i + 1, gop);
        gops.push_back(gop);
    }
    return true;
}

}  // namespace h264
