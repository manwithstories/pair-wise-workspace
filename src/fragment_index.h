#pragma once

#include "box_tree.h"
#include "init_segment.h"

#include <cstdint>
#include <string>
#include <vector>

namespace fmp4 {

struct SampleRecord {
    bool duration_present = false;
    bool size_present = false;
    bool flags_present = false;
    bool composition_time_present = false;
    uint32_t duration = 0;
    uint32_t size = 0;
    uint32_t flags = 0;
    int32_t composition_time_offset = 0;
    uint64_t file_offset = 0;
};

struct TrunRecord {
    uint8_t version = 0;
    uint32_t flags = 0;
    uint32_t sample_count = 0;
    bool has_data_offset = false;
    int64_t data_offset = 0;
    uint64_t data_start = 0;
    std::vector<SampleRecord> samples;
};

struct TfhdRecord {
    uint8_t version = 0;
    uint32_t flags = 0;
    uint32_t track_id = 0;
    bool has_base_data_offset = false;
    uint64_t base_data_offset = 0;
    bool has_sample_description_index = false;
    uint32_t sample_description_index = 0;
    bool has_default_sample_duration = false;
    uint32_t default_sample_duration = 0;
    bool has_default_sample_size = false;
    uint32_t default_sample_size = 0;
    bool has_default_sample_flags = false;
    uint32_t default_sample_flags = 0;
};

struct TrafRecord {
    TfhdRecord tfhd;
    uint8_t tfdt_version = 0;
    uint64_t tfdt_base = 0;
    std::vector<TrunRecord> truns;
    uint64_t sampleCount() const;
    uint64_t sampleDuration() const;
};

struct FragmentRecord {
    uint32_t sequence = 0;
    uint64_t moof_offset = 0;
    uint64_t moof_size = 0;
    bool has_mdat = false;
    uint64_t mdat_offset = 0;
    uint64_t mdat_size = 0;
    uint64_t mdat_header_size = 0;
    std::vector<TrafRecord> trafs;
};

class FragmentIndex {
public:
    static FragmentIndex parse(const BoxReader& reader, const BoxTree& tree,
                               const InitSegment* init = nullptr);

    const std::vector<FragmentRecord>& fragments() const { return fragments_; }

private:
    std::vector<FragmentRecord> fragments_;
};

}  // namespace fmp4

