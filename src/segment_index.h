#pragma once

#include "init_parse.h"
#include "json.h"

#include <cstdint>
#include <set>
#include <string>
#include <vector>

namespace mp4 {

struct SegmentTrack {
    uint32_t track_id = 0;
    uint32_t timescale = 0;
    uint8_t tfdt_version = 0;
    uint64_t tfdt_raw = 0;
    uint64_t base_decode_time = 0;
    uint64_t duration = 0;
    uint32_t sample_count = 0;
    bool indexable = true;
    bool trun_signed = false;
    std::vector<uint8_t> trun_versions;
    int64_t composition_offset_min = 0;
    int64_t composition_offset_max = 0;
};

struct SegmentInfo {
    std::string file;
    uint32_t sequence = 0;
    size_t natural_order = 0;
    bool duplicate = false;
    bool out_of_order = false;
    uint64_t moof_offset = 0;
    uint64_t moof_size = 0;
    uint64_t mdat_offset = 0;
    uint64_t mdat_size = 0;
    std::vector<SegmentTrack> tracks;
};

struct TrackSummary {
    uint32_t track_id = 0;
    uint32_t timescale = 0;
    std::string handler;
    std::string codec;
    std::set<uint8_t> tfdt_versions;
    std::set<uint8_t> trun_versions;
    uint32_t unresolved_segments = 0;
};

struct SegmentIndex {
    std::string source_dir;
    std::string init_file;
    InitInfo init;
    uint32_t segments_found = 0;
    uint32_t segments_after_repack = 0;
    std::vector<uint32_t> missing_sequences;
    std::vector<uint32_t> duplicate_sequences;
    uint32_t duplicate_count = 0;
    uint32_t out_of_order_count = 0;
    bool seq_contiguous = false;
    std::vector<SegmentInfo> segments;
    std::vector<TrackSummary> tracks;
};

SegmentIndex build_index(const std::string& directory);
Json index_to_json(const SegmentIndex& index);
SegmentIndex index_from_json(const Json& root);
InitInfo init_from_json(const Json& root, const std::string& source_path = {});
Json index_to_json_file(const SegmentIndex& index);

}  // namespace mp4
