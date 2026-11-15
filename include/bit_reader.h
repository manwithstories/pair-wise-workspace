#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace h264scan {

class BitReader {
public:
    explicit BitReader(const std::vector<uint8_t>& data);

    bool readBit(bool& value);
    bool readBits(unsigned int count, uint32_t& value);
    bool readUE(uint32_t& value);
    bool readSE(int32_t& value);
    bool skipBits(uint64_t count);

    size_t bitPosition() const { return bit_position_; }
    size_t bitsRemaining() const;
    bool failed() const { return failed_; }
    const std::string& error() const { return error_; }

    static bool removeEmulationPrevention(const std::vector<uint8_t>& ebsp,
                                          std::vector<uint8_t>& rbsp,
                                          std::string& error);

private:
    bool fail(const char* message);

    const std::vector<uint8_t>& data_;
    size_t bit_position_ = 0;
    bool failed_ = false;
    std::string error_;
};

}  // namespace h264scan
