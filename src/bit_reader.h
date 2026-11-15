#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace h264 {

class BitReader {
public:
    BitReader(const std::uint8_t* data, std::size_t size);

    bool ok() const { return error_.empty(); }
    const std::string& error() const { return error_; }
    std::size_t bits_left() const { return bytes_.size() * 8 - bit_pos_; }
    std::size_t bit_position() const { return bit_pos_; }
    bool byte_aligned() const { return (bit_pos_ & 7U) == 0; }

    std::uint64_t u(int bits);
    std::uint64_t ue();
    std::int64_t se();
    bool skip(std::size_t bits);

private:
    bool read_bit(std::uint64_t& value);
    void fail(const std::string& message);

    std::vector<std::uint8_t> bytes_;
    std::size_t bit_pos_ = 0;
    std::string error_;
};

}  // namespace h264
