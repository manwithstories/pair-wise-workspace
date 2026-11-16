#include "bit_reader.h"

#include <string>

namespace h264 {

uint32_t BitReader::readBits(unsigned n) {
  if (n == 0 || n > 32)
    throw BitReaderError(BitReaderError::INVALID_ARG, "readBits: n must be in 1..32");
  if (bit_pos_ + n > size_bits_)
    throw BitReaderError(BitReaderError::OUT_OF_BITS, "readBits: bitstream exhausted");

  uint32_t value = 0;
  for (unsigned i = 0; i < n; ++i) {
    const size_t byte_index = bit_pos_ >> 3;
    const unsigned bit_index = 7u - static_cast<unsigned>(bit_pos_ & 7u);
    const uint32_t bit = (data_[byte_index] >> bit_index) & 1u;
    value = (value << 1) | bit;
    ++bit_pos_;
  }
  return value;
}

bool BitReader::tryReadBits(unsigned n, uint32_t &out) {
  try {
    out = readBits(n);
    return true;
  } catch (const BitReaderError &) {
    return false;
  }
}

std::vector<uint8_t> BitReader::removeEmulationBytes(const uint8_t *data,
                                                     size_t size,
                                                     bool &had_emulation) {
  std::vector<uint8_t> rbsp;
  rbsp.reserve(size);
  had_emulation = false;

  int zero_run = 0;
  for (size_t i = 0; i < size; ++i) {
    const uint8_t b = data[i];
    // H.264 9.1：00 00 03 序列里的 03 是防竞争字节，一律丢弃，
    // 无论其后跟随的是 00/01/02/03。
    if (zero_run >= 2 && b == 0x03) {
      had_emulation = true;
      zero_run = 0;
      continue;
    }
    rbsp.push_back(b);
    zero_run = (b == 0x00) ? zero_run + 1 : 0;
  }
  return rbsp;
}

} // namespace h264
