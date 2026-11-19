#pragma once

// Big-endian byte helpers shared by the container writer and parser.

#include <cstddef>
#include <cstdint>
#include <span>
#include <vector>

namespace mfs {

class ByteReader {
 public:
  explicit ByteReader(std::span<const uint8_t> data) : data_(data) {}

  bool has(size_t count) const { return pos_ + count <= data_.size(); }
  size_t position() const { return pos_; }
  size_t remaining() const { return data_.size() - pos_; }
  void skip(size_t count) { pos_ += count; }

  uint8_t u8();
  uint16_t u16();
  uint32_t u32();
  uint64_t u64();
  std::span<const uint8_t> bytes(size_t count);

 private:
  std::span<const uint8_t> data_;
  size_t pos_{0};
};

void put_u8(std::vector<uint8_t>& out, uint8_t value);
void put_u16(std::vector<uint8_t>& out, uint16_t value);
void put_u32(std::vector<uint8_t>& out, uint32_t value);
void put_u64(std::vector<uint8_t>& out, uint64_t value);
void put_bytes(std::vector<uint8_t>& out, std::span<const uint8_t> bytes);

}  // namespace mfs
