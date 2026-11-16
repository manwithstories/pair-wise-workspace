#pragma once

#include "nal_scanner.h"
#include "pps_parser.h"
#include "slice_parser.h"
#include "sps_parser.h"

#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace h264 {

struct Anomaly {
    std::uint64_t offset = 0;
    std::string kind;
    std::string message;
};

struct SliceRecord {
    std::uint64_t offset = 0;
    SliceInfo info;
};

struct FrameRecord {
    std::uint64_t offset = 0;
    std::uint64_t end_offset = 0;
    std::uint32_t frame_num = 0;
    FrameType frame_type = FrameType::Unknown;
    bool idr = false;
    bool field_pic_flag = false;
    bool bottom_field_flag = false;
    bool interlaced = false;
    std::uint32_t pic_order_cnt_lsb = 0;
    std::vector<SliceRecord> slices;

    std::size_t sliceCount() const { return slices.size(); }
};

struct Gop {
    std::uint32_t sequence_id = 0;
    std::uint64_t start_offset = 0;
    std::uint64_t end_offset = 0;
    std::size_t frame_count = 0;
    std::size_t i_count = 0;
    std::size_t p_count = 0;
    std::size_t b_count = 0;
    std::size_t slice_count = 0;
    bool interlaced = false;
    std::vector<FrameRecord> frames;
};

struct Sequence {
    std::uint32_t id = 0;
    std::uint64_t start_offset = 0;
    std::uint64_t end_offset = 0;
    std::uint64_t sps_offset = 0;
    Sps sps;
    Pps pps;
    bool has_pps = false;
    std::vector<Gop> gops;
};

struct FileAnalysis {
    std::string path;
    std::uint64_t bytes_read = 0;
    std::size_t nal_count = 0;
    std::vector<Sequence> sequences;
    std::vector<Anomaly> anomalies;
    std::vector<ScanWarning> warnings;
};

struct VerificationIssue {
    std::uint64_t offset = 0;
    std::string message;
};

FileAnalysis analyzeFile(const std::string& path);
std::optional<VerificationIssue> firstProgressivenessIssue(const FileAnalysis& analysis);

std::size_t totalGops(const FileAnalysis& analysis);
std::size_t totalFrames(const FileAnalysis& analysis);
std::size_t totalSlices(const FileAnalysis& analysis);
std::size_t countFrames(const FileAnalysis& analysis, FrameType type);

}  // namespace h264
