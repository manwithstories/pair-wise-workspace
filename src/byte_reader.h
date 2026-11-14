#pragma once

#include <cstddef>
#include <cstdint>
#include <string_view>

namespace mp4 {

class ByteReader {
public:
    ByteReader(const uint8_t* data, size_t size, uint64_t base_offset = 0);

    size_t position() const noexcept { return pos_; }
    size_t size() const noexcept { return size_; }
    size_t remaining() const noexcept { return size_ - pos_; }
    uint64_t absolute_position() const noexcept { return base_offset_ + pos_; }
    const uint8_t* cursor() const noexcept { return data_ + pos_; }

    void seek(size_t position);
    void skip(size_t count);
    uint8_t read_u8();
    uint16_t read_u16();
    uint32_t read_u24();
    uint32_t read_u32();
    uint64_t read_u64();
    int32_t read_i32();
    std::string_view read_bytes(size_t count);
    void read_version_flags(uint8_t& version, uint32_t& flags);

private:
    void require(size_t count) const;

    const uint8_t* data_;
    size_t size_;
    size_t pos_;
    uint64_t base_offset_;
};

}  // namespace mp4
