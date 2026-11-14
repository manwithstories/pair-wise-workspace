#pragma once

#include "segment_index.h"

#include <cstdint>
#include <string>

namespace mp4 {

struct RepackResult {
    uint32_t segments_written = 0;
    bool source_has_segment_issue = false;
};

RepackResult repack_index(const SegmentIndex& index, const std::string& init_path,
                          const std::string& output_path);

}  // namespace mp4
