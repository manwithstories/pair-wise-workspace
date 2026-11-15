#pragma once

#include "h264_headers.h"
#include "picture_assembler.h"

#include <cstdint>
#include <deque>
#include <string>
#include <vector>

namespace h264scan {

struct GopStats {
    uint32_t index = 0;
    uint64_t start_offset = 0;
    uint64_t byte_length = 0;
    uint64_t frames = 0;
    uint64_t i_frames = 0;
    uint64_t p_frames = 0;
    uint64_t b_frames = 0;
    uint64_t field_pairs = 0;
    uint64_t slice_count = 0;
    uint64_t missing_slices = 0;
    uint64_t incomplete_frames = 0;
    uint64_t warnings = 0;
};

struct ReportTotals {
    uint64_t frames = 0;
    uint64_t gop_count = 0;
    uint64_t slice_count = 0;
    uint64_t i_frames = 0;
    uint64_t p_frames = 0;
    uint64_t b_frames = 0;
    uint64_t field_pairs = 0;
    uint64_t missing_slices = 0;
    uint64_t incomplete_frames = 0;
    uint64_t warnings = 0;
};

struct FileReport {
    std::string file;
    bool has_sps = false;
    Sps sps;
    uint64_t nal_count = 0;
    uint64_t sps_count = 0;
    uint64_t pps_count = 0;
    uint64_t unknown_nal_count = 0;
    uint64_t truncated_nal_count = 0;
    std::vector<GopStats> gops;
    ReportTotals totals;
    uint64_t warning_count = 0;
    std::vector<std::string> warnings;
};

class GopReporter {
public:
    explicit GopReporter(std::string file_name);

    bool addFrame(const Frame& frame, std::string& error);
    bool finish(std::string& error);
    void addWarning(const std::string& message);

    FileReport& report() { return report_; }
    const FileReport& report() const { return report_; }

private:
    struct RecentFrame {
        Frame frame;
        uint32_t applied_missing = 0;
        bool deficit_warned = false;
    };

    bool beginGop(const Frame& frame);
    bool finalizeGop(std::string& error);
    bool isPocFallback(const Frame& frame) const;
    void applySliceDeficit(RecentFrame& recent);
    void recomputeTotals();

    FileReport report_;
    bool gop_open_ = false;
    GopStats current_;
    uint64_t last_end_offset_ = 0;
    uint32_t last_poc_lsb_ = 0;
    uint32_t last_frame_num_ = 0;
    uint64_t max_slices_per_frame_ = 0;
    uint64_t gop_warning_start_ = 0;
    std::deque<RecentFrame> recent_frames_;
};

}  // namespace h264scan
