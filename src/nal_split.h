#pragma once

#include "common.h"

#include <cstdint>
#include <fstream>
#include <string>
#include <vector>

namespace h264 {

bool scan_nal_units(const std::string& path,
                    std::vector<NalUnit>& nals,
                    std::uint64_t& file_size,
                    std::string& error);

bool read_nal_payload(std::ifstream& file,
                      const NalUnit& nal,
                      std::vector<std::uint8_t>& payload,
                      std::string& error);

}  // namespace h264
