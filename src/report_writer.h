#pragma once

#include "json.h"
#include "segment_index.h"

#include <cstdint>
#include <string>
#include <vector>

namespace mp4 {

struct TrackDeviation {
    uint32_t track_id = 0;
    std::string name;
    uint32_t timescale = 0;
    double max_deviation_ms = 0.0;
    double average_deviation_ms = 0.0;
    bool monotonic = true;
    uint32_t samples_compared = 0;
};

struct ReportResult {
    uint32_t segments_found = 0;
    uint32_t segments_after_repack = 0;
    std::vector<uint32_t> missing_sequences;
    bool seq_contiguous = false;
    bool repacked_seq_contiguous = false;
    uint32_t tracks = 0;
    std::vector<TrackDeviation> deviations;
    std::string verdict;
};

ReportResult analyze_repacked(const SegmentIndex& index, const std::string& repacked_path);
Json report_to_json(const ReportResult& report);
std::string report_summary(const ReportResult& report);

}  // namespace mp4
