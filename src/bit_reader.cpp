#include "bit_reader.h"

#include <limits>
#include <sstream>

namespace h264 {

BitReader::BitReader(const std::uint8_t* data, std::size_t size) {
    bytes_.reserve(size);
    for (std::size_t i = 0; i < size; ++i) {
        if (i + 2 < size && data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 3) {
            bytes_.push_back(0);
            bytes_.push_back(0);
            i += 2;
        } else {
            bytes_.push_back(data[i]);
        }
    }
}

void BitReader::fail(const std::string& message) {
    if (error_.empty()) {
        std::ostringstream out;
        out << message << " at bit " << bit_pos_;
        error_ = out.str();
    }
}

bool BitReader::read_bit(std::uint64_t& value) {
    if (!error_.empty()) {
        return false;
    }
    if (bit_pos_ >= bytes_.size() * 8) {
        fail("unexpected end of RBSP");
        return false;
    }
    const std::size_t byte_index = bit_pos_ / 8;
    const int shift = 7 - static_cast<int>(bit_pos_ % 8);
    value = (bytes_[byte_index] >> shift) & 1U;
    ++bit_pos_;
    return true;
}

std::uint64_t BitReader::u(int bits) {
    if (bits < 0 || bits > 64) {
        fail("invalid bit width");
        return 0;
    }
    std::uint64_t value = 0;
    for (int i = 0; i < bits; ++i) {
        std::uint64_t bit = 0;
        if (!read_bit(bit)) {
            return 0;
        }
        value = (value << 1U) | bit;
    }
    return value;
}

std::uint64_t BitReader::ue() {
    int leading_zeros = 0;
    std::uint64_t bit = 0;
    while (true) {
        if (!read_bit(bit)) {
            return 0;
        }
        if (bit != 0) {
            break;
        }
        ++leading_zeros;
        if (leading_zeros > 32) {
            fail("Exp-Golomb code has more than 32 leading zero bits");
            return 0;
        }
    }
    if (leading_zeros == 0) {
        return 0;
    }
    const std::uint64_t suffix = u(leading_zeros);
    if (!ok()) {
        return 0;
    }
    return ((std::uint64_t{1} << leading_zeros) - 1U) + suffix;
}

std::int64_t BitReader::se() {
    const std::uint64_t code_num = ue();
    if (!ok()) {
        return 0;
    }
    if ((code_num & 1U) != 0) {
        if (code_num > static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max()) * 2U) {
            fail("signed Exp-Golomb value overflow");
            return 0;
        }
        return static_cast<std::int64_t>((code_num + 1U) / 2U);
    }
    return -static_cast<std::int64_t>(code_num / 2U);
}

bool BitReader::skip(std::size_t bits) {
    if (bits > bits_left()) {
        fail("skip exceeds RBSP");
        return false;
    }
    bit_pos_ += bits;
    return true;
}

}  // namespace h264
