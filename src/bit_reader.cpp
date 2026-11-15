#include "bit_reader.h"

#include <limits>

namespace h264scan {

BitReader::BitReader(const std::vector<uint8_t>& data) : data_(data) {}

bool BitReader::fail(const char* message) {
    if (!failed_) {
        failed_ = true;
        error_ = message;
    }
    return false;
}

size_t BitReader::bitsRemaining() const {
    const size_t total_bits = data_.size() * 8U;
    return bit_position_ <= total_bits ? total_bits - bit_position_ : 0U;
}

bool BitReader::readBit(bool& value) {
    if (bit_position_ >= data_.size() * 8U) {
        return fail("unexpected end of RBSP");
    }
    const size_t byte_index = bit_position_ / 8U;
    const unsigned int shift = 7U - static_cast<unsigned int>(bit_position_ % 8U);
    value = ((data_[byte_index] >> shift) & 1U) != 0U;
    ++bit_position_;
    return true;
}

bool BitReader::readBits(unsigned int count, uint32_t& value) {
    if (count > 32U) {
        return fail("bit read wider than 32 bits");
    }
    if (count > bitsRemaining()) {
        return fail("bit read exceeds RBSP");
    }

    uint32_t result = 0;
    for (unsigned int i = 0; i < count; ++i) {
        bool bit = false;
        if (!readBit(bit)) {
            return false;
        }
        result = (result << 1U) | (bit ? 1U : 0U);
    }
    value = result;
    return true;
}

bool BitReader::readUE(uint32_t& value) {
    unsigned int leading_zeros = 0;
    bool stop_bit = false;

    while (true) {
        if (!readBit(stop_bit)) {
            return false;
        }
        if (stop_bit) {
            break;
        }
        if (leading_zeros >= 31U) {
            return fail("UE(v) value exceeds 32-bit range");
        }
        ++leading_zeros;
    }

    uint32_t suffix = 0;
    if (leading_zeros != 0U && !readBits(leading_zeros, suffix)) {
        return false;
    }

    const uint64_t base = (uint64_t{1} << leading_zeros) - 1U;
    const uint64_t decoded = base + suffix;
    if (decoded > std::numeric_limits<uint32_t>::max()) {
        return fail("UE(v) value overflows uint32");
    }
    value = static_cast<uint32_t>(decoded);
    return true;
}

bool BitReader::readSE(int32_t& value) {
    uint32_t code_num = 0;
    if (!readUE(code_num)) {
        return false;
    }

    int64_t signed_value = 0;
    if ((code_num & 1U) == 0U) {
        signed_value = -static_cast<int64_t>(code_num / 2U);
    } else {
        signed_value = static_cast<int64_t>((static_cast<uint64_t>(code_num) + 1U) / 2U);
    }

    if (signed_value < std::numeric_limits<int32_t>::min() ||
        signed_value > std::numeric_limits<int32_t>::max()) {
        return fail("SE(v) value overflows int32");
    }
    value = static_cast<int32_t>(signed_value);
    return true;
}

bool BitReader::skipBits(uint64_t count) {
    if (count > bitsRemaining()) {
        return fail("skip exceeds RBSP");
    }
    bit_position_ += static_cast<size_t>(count);
    return true;
}

bool BitReader::removeEmulationPrevention(const std::vector<uint8_t>& ebsp,
                                          std::vector<uint8_t>& rbsp,
                                          std::string& error) {
    rbsp.clear();
    rbsp.reserve(ebsp.size());

    size_t zero_count = 0;
    for (size_t i = 0; i < ebsp.size(); ++i) {
        const uint8_t byte = ebsp[i];
        if (zero_count >= 2U && byte == 0x03U) {
            if (i + 1U >= ebsp.size()) {
                error = "truncated emulation prevention sequence";
                return false;
            }
            zero_count = 0;
            continue;
        }

        rbsp.push_back(byte);
        if (byte == 0x00U) {
            ++zero_count;
        } else {
            zero_count = 0;
        }
    }
    return true;
}

}  // namespace h264scan
