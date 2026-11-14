#include "byte_reader.h"

#include "error.h"

#include <sstream>

namespace mp4 {

ByteReader::ByteReader(const uint8_t* data, size_t size, uint64_t base_offset)
    : data_(data), size_(size), pos_(0), base_offset_(base_offset) {}

void ByteReader::require(size_t count) const {
    if (count > size_ - pos_) {
        std::ostringstream oss;
        oss << "truncated input at byte offset " << (base_offset_ + pos_)
            << ": need " << count << " byte(s), have " << (size_ - pos_);
        fail(ExitCode::FormatError, oss.str());
    }
}

void ByteReader::seek(size_t position) {
    if (position > size_) {
        std::ostringstream oss;
        oss << "seek out of bounds at byte offset " << (base_offset_ + position);
        fail(ExitCode::FormatError, oss.str());
    }
    pos_ = position;
}

void ByteReader::skip(size_t count) {
    require(count);
    pos_ += count;
}

uint8_t ByteReader::read_u8() {
    require(1);
    return data_[pos_++];
}

uint16_t ByteReader::read_u16() {
    require(2);
    const uint16_t value = static_cast<uint16_t>(
        (static_cast<uint16_t>(data_[pos_]) << 8) | data_[pos_ + 1]);
    pos_ += 2;
    return value;
}

uint32_t ByteReader::read_u24() {
    require(3);
    const uint32_t value = (static_cast<uint32_t>(data_[pos_]) << 16) |
                           (static_cast<uint32_t>(data_[pos_ + 1]) << 8) |
                           data_[pos_ + 2];
    pos_ += 3;
    return value;
}

uint32_t ByteReader::read_u32() {
    require(4);
    const uint32_t value = (static_cast<uint32_t>(data_[pos_]) << 24) |
                           (static_cast<uint32_t>(data_[pos_ + 1]) << 16) |
                           (static_cast<uint32_t>(data_[pos_ + 2]) << 8) |
                           data_[pos_ + 3];
    pos_ += 4;
    return value;
}

uint64_t ByteReader::read_u64() {
    const uint64_t high = read_u32();
    const uint64_t low = read_u32();
    return (high << 32) | low;
}

int32_t ByteReader::read_i32() {
    return static_cast<int32_t>(read_u32());
}

std::string_view ByteReader::read_bytes(size_t count) {
    require(count);
    const std::string_view view(reinterpret_cast<const char*>(data_ + pos_), count);
    pos_ += count;
    return view;
}

void ByteReader::read_version_flags(uint8_t& version, uint32_t& flags) {
    version = read_u8();
    flags = read_u24();
}

}  // namespace mp4
