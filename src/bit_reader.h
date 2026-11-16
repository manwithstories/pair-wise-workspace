#pragma once
// BitReader：H.264 RBSP 位流读取器（大端、MSB 优先）。
// 仅持有外部数据的指针与长度，不拥有数据、不复制码流。
#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <vector>

namespace h264 {

class BitReaderError : public std::runtime_error {
public:
  enum Code { OUT_OF_BITS, INVALID_ARG };
  explicit BitReaderError(Code code, const std::string &msg)
      : std::runtime_error(msg), code_(code) {}
  Code code() const { return code_; }

private:
  Code code_;
};

class BitReader {
public:
  BitReader() = default;
  BitReader(const uint8_t *data, size_t size_bytes)
      : data_(data), size_bits_(size_bytes * 8u), bit_pos_(0) {}

  // 读取 1..32 位；参数非法或位流耗尽时抛 BitReaderError。
  uint32_t readBits(unsigned n);
  bool readBit() { return readBits(1) != 0; }

  // 不抛异常的版本：失败返回 false，供 tolerant 模式累计 warning。
  bool tryReadBits(unsigned n, uint32_t &out);

  void byteAlign() { bit_pos_ = (bit_pos_ + 7u) & ~static_cast<size_t>(7u); }

  size_t bitPos() const { return bit_pos_; }
  size_t bitsLeft() const {
    return bit_pos_ >= size_bits_ ? 0 : size_bits_ - bit_pos_;
  }
  bool eof() const { return bit_pos_ >= size_bits_; }
  size_t sizeBytes() const { return size_bits_ / 8u; }

  // RBSP -> EBSP 逆操作：移除 emulation prevention byte（00 00 03 中的 03）。
  // 返回新分配的 RBSP 缓冲；had_emulation 报告是否发生过移除。
  static std::vector<uint8_t> removeEmulationBytes(const uint8_t *data,
                                                   size_t size,
                                                   bool &had_emulation);

private:
  const uint8_t *data_ = nullptr;
  size_t size_bits_ = 0;
  size_t bit_pos_ = 0;
};

} // namespace h264
