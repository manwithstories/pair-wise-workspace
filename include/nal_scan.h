#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <string>
#include <vector>

namespace h264scan {

struct NalUnit {
    uint8_t nal_ref_idc = 0;
    uint8_t nal_unit_type = 0;
    uint64_t start_offset = 0;
    uint64_t header_offset = 0;
    uint64_t end_offset = 0;
    bool at_eof = false;
    std::vector<uint8_t> data;

    uint8_t header() const { return data.empty() ? 0U : data.front(); }
};

struct ScanStats {
    uint64_t nal_count = 0;
    uint64_t bytes_read = 0;
    uint64_t empty_nal_count = 0;
};

using NalCallback = std::function<bool(const NalUnit&)>;

bool scanAnnexBFile(const std::string& path, const NalCallback& callback,
                    ScanStats& stats, std::string& error,
                    size_t chunk_size = 64U * 1024U);

}  // namespace h264scan
