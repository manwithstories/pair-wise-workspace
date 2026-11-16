#include "exp_golomb.h"

#include <limits>
#include <sstream>

namespace h264 {

namespace {
std::size_t offsetOf(const BitReader& reader) {
    return static_cast<std::size_t>(reader.bitPosition());
}
}  // namespace

std::uint32_t readUe(BitReader& reader) {
    const std::size_t start = offsetOf(reader);
    unsigned leadingZeros = 0;
    while (!reader.readBit()) {
        ++leadingZeros;
        if (leadingZeros > 32) {
            throw ParseError("ue leading zeros exceed 32", start);
        }
    }

    const std::uint64_t suffix = reader.readBits(leadingZeros);
    const std::uint64_t mask = (std::uint64_t{1} << leadingZeros) - 1;
    const std::uint64_t codeNum = mask + suffix;
    if (codeNum > std::numeric_limits<std::uint32_t>::max()) {
        throw ParseError("ue value out of range", start);
    }
    return static_cast<std::uint32_t>(codeNum);
}

std::int32_t readSe(BitReader& reader) {
    const std::size_t start = offsetOf(reader);
    const std::uint64_t codeNum = readUe(reader);
    std::int64_t value = 0;
    if ((codeNum & 1U) != 0) {
        value = static_cast<std::int64_t>((codeNum + 1U) / 2U);
    } else {
        value = -static_cast<std::int64_t>(codeNum / 2U);
    }
    if (value < std::numeric_limits<std::int32_t>::min() ||
        value > std::numeric_limits<std::int32_t>::max()) {
        throw ParseError("se value out of range", start);
    }
    return static_cast<std::int32_t>(value);
}

std::uint32_t readTe(BitReader& reader, std::uint32_t range) {
    if (range == 0) {
        throw ParseError("te range must be positive", offsetOf(reader));
    }
    if (range == 1) {
        return reader.readBit() ? 0U : 1U;
    }
    const std::size_t start = offsetOf(reader);
    const std::uint32_t value = readUe(reader);
    if (value > range) {
        std::ostringstream message;
        message << "te value " << value << " exceeds range " << range;
        throw ParseError(message.str(), start);
    }
    return value;
}

}  // namespace h264
