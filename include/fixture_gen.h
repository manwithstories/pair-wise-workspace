#pragma once

#include <cstdint>
#include <string>

namespace h264scan {

struct SynthOptions {
    std::string output_path;
    uint32_t gops = 4;
    uint32_t slices = 3;
    bool interlaced_pair = true;
    uint64_t seed = 7;
};

bool validateSynthOptions(const SynthOptions& options, std::string& error);
bool generateFixture(const SynthOptions& options, std::string& error);

}  // namespace h264scan
