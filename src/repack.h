#pragma once

#include <cstdint>
#include <string>

namespace fmp4 {

struct RepackOptions {
    bool renumber = false;
    uint8_t tfdt_version = 1;
};

struct RepackStats {
    uint64_t fragment_count = 0;
    uint64_t bytes_written = 0;
};

RepackStats repackFile(const std::string& input_path, const std::string& output_path,
                       const RepackOptions& options);

}  // namespace fmp4

