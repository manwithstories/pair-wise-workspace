#pragma once

#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <string>
#include <vector>

namespace h264 {

class ParseError : public std::runtime_error {
public:
    ParseError(const std::string& message, std::size_t bitOffset);
    std::size_t bitOffset() const noexcept { return bitOffset_; }

private:
    std::size_t bitOffset_;
};

class BitReader {
public:
    BitReader(const std::uint8_t* data, std::size_t size);
    explicit BitReader(const std::vector<std::uint8_t>& data);

    bool readBit();
    std::uint64_t readBits(unsigned count);
    void skipBits(std::uint64_t count);
    void byteAlign();

    std::uint64_t bitPosition() const noexcept { return bitPosition_; }
    std::uint64_t bitsRemaining() const noexcept { return bitSize_ - bitPosition_; }
    std::size_t size() const noexcept { return size_; }
    const std::uint8_t* data() const noexcept { return data_; }

private:
    const std::uint8_t* data_;
    std::size_t size_;
    std::uint64_t bitSize_;
    std::uint64_t bitPosition_ = 0;
};

}  // namespace h264
