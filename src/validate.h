#pragma once

#include "fragment_index.h"
#include "init_segment.h"

#include <cstdint>
#include <string>
#include <vector>

namespace fmp4 {

struct MissingFragment {
    uint32_t sequence = 0;
    bool has_previous = false;
    uint32_t previous_sequence = 0;
    uint64_t previous_moof_offset = 0;
    bool has_next = false;
    uint32_t next_sequence = 0;
    uint64_t next_moof_offset = 0;
};

struct SequenceReport {
    bool contiguous = false;
    bool in_order = false;
    std::vector<uint32_t> duplicate;
    std::vector<uint32_t> missing;
    std::vector<uint32_t> out_of_order;
    std::vector<MissingFragment> missing_details;
};

struct TrackReport {
    uint32_t track_id = 0;
    uint32_t timescale = 0;
    std::string codec;
    bool has_trex = false;
    uint32_t default_sample_description_index = 0;
    uint32_t default_sample_duration = 0;
    uint32_t default_sample_size = 0;
    uint32_t default_sample_flags = 0;
    uint64_t fragment_occurrences = 0;
    uint64_t sample_count = 0;
    double deviation_max_ticks = 0.0;
    double deviation_mean_ticks = 0.0;
    double deviation_max_ms = 0.0;
    double deviation_mean_ms = 0.0;
};

struct ValidationResult {
    InitSegment init;
    uint64_t fragment_count = 0;
    SequenceReport sequence;
    std::vector<TrackReport> tracks;
    std::vector<InitMismatch> init_diffs;
    bool init_checked = false;
    bool init_compatible = true;
    bool timing_monotonic = true;
    bool passed = false;
};

ValidationResult validateMedia(const BoxReader& reader, const BoxTree& tree,
                               const InitSegment* expected_init = nullptr);

}  // namespace fmp4

