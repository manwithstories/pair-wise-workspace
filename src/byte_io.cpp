#include "byte_io.hpp"

#include <stdexcept>

namespace mfs {

uint8_t ByteReader::u8() {
  if (!has(1)) {
    throw std::out_of_range("byte reader underflow");
  }
  return data_[pos_++];
}

uint16_t ByteReader::u16() {
  if (!has(2)) {
    throw std::out_of_range("byte reader underflow");
  }
  const uint16_t value = static_cast<uint16_t>(uint16_t{data_[pos_]} << 8 |
                                               uint16_t{data_[pos_ + 1]});
  pos_ += 2;
  return value;
}

uint32_t ByteReader::u32() {
  if (!has(4)) {
    throw std::out_of_range("byte reader underflow");
  }
  const uint32_t value = (uint32_t{data_[pos_]} << 24) |
                         (uint32_t{data_[pos_ + 1]} << 16) |
                         (uint32_t{data_[pos_ + 2]} << 8) |
                         uint32_t{data_[pos_ + 3]};
  pos_ += 4;
  return value;
}

uint64_t ByteReader::u64() {
  const uint64_t high = u32();
  const uint64_t low = u32();
  return (high << 32) | low;
}

std::span<const uint8_t> ByteReader::bytes(size_t count) {
  if (!has(count)) {
    throw std::out_of_range("byte reader underflow");
  }
  const auto view = data_.subspan(pos_, count);
  pos_ += count;
  return view;
}

void put_u8(std::vector<uint8_t>& out, uint8_t value) { out.push_back(value); }

void put_u16(std::vector<uint8_t>& out, uint16_t value) {
  out.push_back(static_cast<uint8_t>(value >> 8));
  out.push_back(static_cast<uint8_t>(value));
}

void put_u32(std::vector<uint8_t>& out, uint32_t value) {
  out.push_back(static_cast<uint8_t>(value >> 24));
  out.push_back(static_cast<uint8_t>(value >> 16));
  out.push_back(static_cast<uint8_t>(value >> 8));
  out.push_back(static_cast<uint8_t>(value));
}

void put_u64(std::vector<uint8_t>& out, uint64_t value) {
  put_u32(out, static_cast<uint32_t>(value >> 32));
  put_u32(out, static_cast<uint32_t>(value));
}

void put_bytes(std::vector<uint8_t>& out, std::span<const uint8_t> bytes) {
  out.insert(out.end(), bytes.begin(), bytes.end());
}

}  // namespace mfs
