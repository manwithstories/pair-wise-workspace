#pragma once

#include "box_tree.h"

#include <cstdint>
#include <string>
#include <vector>

namespace fmp4 {

struct TrackInit {
    uint32_t track_id = 0;
    uint32_t timescale = 0;
    std::string codec;
    std::string handler;
    bool has_trex = false;
    uint32_t default_sample_description_index = 0;
    uint32_t default_sample_duration = 0;
    uint32_t default_sample_size = 0;
    uint32_t default_sample_flags = 0;
};

struct InitSegment {
    std::vector<TrackInit> tracks;

    static InitSegment parse(const BoxReader& reader, const BoxTree& tree);
};

struct InitMismatch {
    std::string field;
    std::string left;
    std::string right;
};

std::vector<InitMismatch> compareInitSegments(const InitSegment& left,
                                              const InitSegment& right);

}  // namespace fmp4

