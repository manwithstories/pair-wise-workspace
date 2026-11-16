#include "bit_writer.h"

namespace h264 {

void BitWriter::writeBits(uint32_t value, unsigned n) {
  for (unsigned i = n; i > 0; --i) {
    const unsigned bit = (value >> (i - 1)) & 1u;
    if (bit_pos_ % 8 == 0)
      buf_.push_back(0);
    if (bit)
      buf_.back() |= static_cast<uint8_t>(1u << (7 - (bit_pos_ % 8)));
    ++bit_pos_;
  }
}

void BitWriter::writeUE(uint32_t code_num) {
  // codeNum+1 的二进制长度即码长，leadingZeroBits = (len-1)/2
  unsigned code_len = 1;
  uint32_t v = code_num + 1;
  while (v > 1) {
    v >>= 1;
    code_len += 2;
  }
  writeBits(code_num + 1, code_len);
}

void BitWriter::writeSE(int32_t signed_value) {
  uint32_t code_num;
  if (signed_value > 0)
    code_num = static_cast<uint32_t>(2 * signed_value - 1);
  else
    code_num = static_cast<uint32_t>(-2 * signed_value);
  writeUE(code_num);
}

void BitWriter::writeRbspTrailingBits() {
  writeBit(true); // rbsp_stop_one_bit
  while (bit_pos_ % 8 != 0)
    writeBit(false); // rbsp_alignment_zero_bit
}

std::vector<uint8_t>
BitWriter::addEmulationBytes(const std::vector<uint8_t> &rbsp) {
  std::vector<uint8_t> ebsp;
  ebsp.reserve(rbsp.size() + rbsp.size() / 32 + 1);
  int zero_run = 0;
  for (uint8_t b : rbsp) {
    if (zero_run >= 2 && b <= 0x03) {
      ebsp.push_back(0x03); // emulation_prevention_three_byte
      zero_run = 0;
    }
    ebsp.push_back(b);
    zero_run = (b == 0x00) ? zero_run + 1 : 0;
  }
  return ebsp;
}

} // namespace h264
