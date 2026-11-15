#include "box_reader.h"

#include <algorithm>
#include <array>
#include <limits>
#include <sstream>

namespace fmp4 {

namespace {

uint64_t addChecked(uint64_t a, uint64_t b, uint64_t offset) {
    if (b > std::numeric_limits<uint64_t>::max() - a) {
        throw parseError("integer overflow while reading box", offset);
    }
    return a + b;
}

}  // namespace

BoxReader::BoxReader(const std::string& path) : path_(path), input_(path, std::ios::binary) {
    if (!input_) {
        throw ioError("cannot open input file: " + path);
    }
    input_.seekg(0, std::ios::end);
    const std::streamoff end = input_.tellg();
    if (end < 0) {
        throw ioError("cannot determine size of input file: " + path);
    }
    file_size_ = static_cast<uint64_t>(end);
    input_.seekg(0, std::ios::beg);
    if (!input_) {
        throw ioError("cannot seek input file: " + path);
    }
}

BoxHeader BoxReader::readBoxHeader(uint64_t offset, uint64_t limit) const {
    if (limit > file_size_) {
        limit = file_size_;
    }
    if (offset > limit) {
        throw parseError("box offset is past enclosing boundary", offset);
    }
    if (limit - offset < 8) {
        throw parseError("truncated box header", offset);
    }
    const std::vector<uint8_t> header = read(offset, std::min<uint64_t>(32, limit - offset));
    uint64_t size = readU32(header.data());
    BoxHeader result;
    result.offset = offset;
    result.type = readFourCC(header.data() + 4);
    result.header_size = 8;

    if (size == 1) {
        if (limit - offset < 16) {
            throw parseError("truncated 64-bit box header", offset);
        }
        result.header_size = 16;
        size = readU64(header.data() + 8);
    } else if (size == 0) {
        size = limit - offset;
    }

    if (result.type == "uuid") {
        if (size < result.header_size + 16 || limit - offset < result.header_size + 16) {
            throw parseError("truncated uuid box header", offset);
        }
        result.header_size += 16;
    }
    if (size < result.header_size) {
        throw parseError("box size is smaller than its header", offset);
    }
    if (size > limit - offset) {
        throw parseError("box extends beyond enclosing boundary", offset);
    }
    result.size = size;
    return result;
}

std::vector<uint8_t> BoxReader::read(uint64_t offset, uint64_t length) const {
    const uint64_t end = addChecked(offset, length, offset);
    if (end > file_size_) {
        throw parseError("truncated file while reading metadata", file_size_);
    }
    if (length == 0) {
        return {};
    }
    input_.clear();
    input_.seekg(static_cast<std::streamoff>(offset), std::ios::beg);
    if (!input_) {
        throw ioError("seek failed in input file: " + path_);
    }
    std::vector<uint8_t> bytes(static_cast<size_t>(length));
    input_.read(reinterpret_cast<char*>(bytes.data()), static_cast<std::streamsize>(length));
    if (static_cast<uint64_t>(input_.gcount()) != length) {
        throw parseError("short read while reading metadata", offset);
    }
    return bytes;
}

void BoxReader::copyRange(uint64_t offset, uint64_t length, std::ostream& out) const {
    const uint64_t end = addChecked(offset, length, offset);
    if (end > file_size_) {
        throw parseError("truncated file while copying payload", file_size_);
    }
    input_.clear();
    input_.seekg(static_cast<std::streamoff>(offset), std::ios::beg);
    if (!input_) {
        throw ioError("seek failed in input file: " + path_);
    }
    std::array<char, 64 * 1024> buffer{};
    uint64_t left = length;
    while (left > 0) {
        const std::streamsize chunk = static_cast<std::streamsize>(std::min<uint64_t>(left, buffer.size()));
        input_.read(buffer.data(), chunk);
        if (input_.gcount() != chunk) {
            throw parseError("short read while copying payload", offset + (length - left));
        }
        out.write(buffer.data(), chunk);
        if (!out) {
            throw ioError("failed to write output payload");
        }
        left -= static_cast<uint64_t>(chunk);
    }
}

uint16_t readU16(const uint8_t* data) {
    return static_cast<uint16_t>((static_cast<uint16_t>(data[0]) << 8) | data[1]);
}

uint32_t readU32(const uint8_t* data) {
    return (static_cast<uint32_t>(data[0]) << 24) | (static_cast<uint32_t>(data[1]) << 16) |
           (static_cast<uint32_t>(data[2]) << 8) | static_cast<uint32_t>(data[3]);
}

uint64_t readU64(const uint8_t* data) {
    return (static_cast<uint64_t>(readU32(data)) << 32) | readU32(data + 4);
}

int32_t readI32(const uint8_t* data) {
    return static_cast<int32_t>(readU32(data));
}

std::string readFourCC(const uint8_t* data) {
    std::string result(4, '\0');
    for (size_t i = 0; i < 4; ++i) {
        result[i] = static_cast<char>(data[i]);
    }
    return result;
}

void ByteReader::need(uint64_t count) const {
    if (count > remaining()) {
        throw parseError("truncated box payload", base_ + position_);
    }
}

void ByteReader::seek(uint64_t position) {
    if (position > size_) {
        throw parseError("box payload offset is out of range", base_ + position);
    }
    position_ = position;
}

void ByteReader::skip(uint64_t count) {
    need(count);
    position_ += count;
}

uint8_t ByteReader::u8() {
    need(1);
    return data_[position_++];
}

uint16_t ByteReader::u16() {
    need(2);
    const uint16_t value = readU16(data_ + position_);
    position_ += 2;
    return value;
}

uint32_t ByteReader::u32() {
    need(4);
    const uint32_t value = readU32(data_ + position_);
    position_ += 4;
    return value;
}

uint64_t ByteReader::u64() {
    need(8);
    const uint64_t value = readU64(data_ + position_);
    position_ += 8;
    return value;
}

int32_t ByteReader::i32() {
    need(4);
    const int32_t value = readI32(data_ + position_);
    position_ += 4;
    return value;
}

std::string ByteReader::fourCC() {
    need(4);
    const std::string value = readFourCC(data_ + position_);
    position_ += 4;
    return value;
}

const uint8_t* ByteReader::bytes(uint64_t count) {
    need(count);
    const uint8_t* value = data_ + position_;
    position_ += count;
    return value;
}

}  // namespace fmp4

