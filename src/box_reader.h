#pragma once

#include "error.h"

#include <cstdint>
#include <fstream>
#include <ostream>
#include <string>
#include <vector>

namespace fmp4 {

struct BoxHeader {
    std::string type;
    uint64_t offset = 0;
    uint64_t size = 0;
    uint64_t header_size = 0;
};

class BoxReader {
public:
    explicit BoxReader(const std::string& path);

    const std::string& path() const { return path_; }
    uint64_t fileSize() const { return file_size_; }

    BoxHeader readBoxHeader(uint64_t offset, uint64_t limit) const;
    std::vector<uint8_t> read(uint64_t offset, uint64_t length) const;
    void copyRange(uint64_t offset, uint64_t length, std::ostream& out) const;

private:
    std::string path_;
    mutable std::ifstream input_;
    uint64_t file_size_ = 0;
};

uint16_t readU16(const uint8_t* data);
uint32_t readU32(const uint8_t* data);
uint64_t readU64(const uint8_t* data);
int32_t readI32(const uint8_t* data);
std::string readFourCC(const uint8_t* data);

class ByteReader {
public:
    ByteReader(const uint8_t* data, size_t size, uint64_t base_offset)
        : data_(data), size_(size), base_(base_offset) {}

    uint64_t position() const { return position_; }
    uint64_t baseOffset() const { return base_; }
    size_t remaining() const { return position_ <= size_ ? size_ - position_ : 0; }
    void seek(uint64_t position);
    void skip(uint64_t count);
    uint8_t u8();
    uint16_t u16();
    uint32_t u32();
    uint64_t u64();
    int32_t i32();
    std::string fourCC();
    const uint8_t* bytes(uint64_t count);

private:
    void need(uint64_t count) const;

    const uint8_t* data_;
    size_t size_;
    uint64_t base_;
    uint64_t position_ = 0;
};

}  // namespace fmp4

