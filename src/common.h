#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace h264 {

struct Warning {
    std::string level;
    std::uint64_t offset = 0;
    std::string message;
};

struct NalUnit {
    std::size_t index = 0;
    std::uint64_t offset = 0;
    std::uint64_t start_code_offset = 0;
    std::uint64_t size = 0;
    std::uint8_t type = 0;
    std::uint8_t nal_ref_idc = 0;
    bool start_code_4byte = false;
    bool at_eof = false;
};

inline const char* slice_type_name(int slice_type) {
    switch (slice_type % 5) {
        case 0: return "P";
        case 1: return "B";
        case 2: return "I";
        case 3: return "SP";
        case 4: return "SI";
        default: return "?";
    }
}

inline const char* nal_type_name(int type) {
    switch (type) {
        case 1: return "slice";
        case 5: return "IDR";
        case 6: return "SEI";
        case 7: return "SPS";
        case 8: return "PPS";
        case 9: return "AUD";
        case 10: return "end_seq";
        case 11: return "end_stream";
        case 12: return "filler";
        default: return "other";
    }
}

}  // namespace h264
