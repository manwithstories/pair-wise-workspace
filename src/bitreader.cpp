#include "bitreader.h"

#include <sstream>
#include <vector>

namespace h264 {

namespace {
std::string withOffset(const std::string& message, std::size_t offset) {
    std::ostringstream out;
    out << message << " (bit offset " << offset << ")";
    return out.str();
}
}  // namespace

ParseError::ParseError(const std::string& message, std::size_t bitOffset)
    : std::runtime_error(withOffset(message, bitOffset)), bitOffset_(bitOffset) {}

BitReader::BitReader(const std::uint8_t* data, std::size_t size)
    : data_(data), size_(size), bitSize_(static_cast<std::uint64_t>(size) * 8) {}

BitReader::BitReader(const std::vector<std::uint8_t>& data)
    : BitReader(data.data(), data.size()) {}

bool BitReader::readBit() {
    if (bitPosition_ >= bitSize_) {
        throw ParseError("unexpected end of RBSP", static_cast<std::size_t>(bitPosition_));
    }
    const std::uint8_t byte = data_[bitPosition_ / 8];
    const unsigned shift = 7U - static_cast<unsigned>(bitPosition_ % 8);
    ++bitPosition_;
    return ((byte >> shift) & 1U) != 0;
}

std::uint64_t BitReader::readBits(unsigned count) {
    if (count > 64) {
        throw ParseError("bit read wider than 64 bits", static_cast<std::size_t>(bitPosition_));
    }
    if (count == 0) {
        return 0;
    }
    if (static_cast<std::uint64_t>(count) > bitsRemaining()) {
        throw ParseError("unexpected end of RBSP", static_cast<std::size_t>(bitPosition_));
    }
    std::uint64_t value = 0;
    for (unsigned i = 0; i < count; ++i) {
        value = (value << 1U) | (readBit() ? 1U : 0U);
    }
    return value;
}

void BitReader::skipBits(std::uint64_t count) {
    if (count > bitsRemaining()) {
        throw ParseError("cannot skip past end of RBSP",
                         static_cast<std::size_t>(bitPosition_));
    }
    bitPosition_ += count;
}

void BitReader::byteAlign() {
    const std::uint64_t rem = bitPosition_ % 8;
    if (rem != 0) {
        skipBits(8 - rem);
    }
}

}  // namespace h264
