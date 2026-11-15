#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace fixture {

using Bytes = std::vector<uint8_t>;

struct Config {
    uint32_t timescale = 90000;
    uint32_t default_duration = 60000;
    uint32_t default_size = 1024;
    uint32_t samples_per_fragment = 3;
    std::string codec = "avc1";
};

Bytes buildInit(const Config& config);
Bytes buildMoof(const Config& config, uint32_t sequence, uint64_t tfdt,
                uint8_t tfdt_version, uint8_t trun_version, bool negative_offset,
                uint64_t moof_offset, uint64_t mdat_header_size);
Bytes buildMdatHeader(const Config& config, uint64_t payload_size, bool large_mdat);
uint64_t fragmentPayloadSize(const Config& config);

}  // namespace fixture

