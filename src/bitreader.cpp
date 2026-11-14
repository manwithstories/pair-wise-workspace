#include "bitreader.h"

namespace h264 {

BitReader::BitReader(const uint8_t* ebsp, size_t size) {
  rbsp_.reserve(size);
  int zero_run = 0;
  for (size_t i = 0; i < size; ++i) {
    uint8_t b = ebsp[i];
    if (zero_run >= 2 && b == 0x03) {
      zero_run = 0;
      continue;
    }
    rbsp_.push_back(b);
    zero_run = (b == 0x00) ? zero_run + 1 : 0;
  }
}

uint32_t BitReader::read_bit() {
  if (bit_pos_ >= rbsp_.size() * 8) {
    fail_parse("bitreader overrun at bit " + std::to_string(bit_pos_));
  }
  uint32_t bit = (rbsp_[bit_pos_ / 8] >> (7 - (bit_pos_ % 8))) & 1u;
  ++bit_pos_;
  return bit;
}

uint32_t BitReader::read_bits(int n) {
  if (n < 0 || n > 32) {
    fail_parse("read_bits with invalid width " + std::to_string(n));
  }
  uint32_t v = 0;
  for (int i = 0; i < n; ++i) {
    v = (v << 1) | read_bit();
  }
  return v;
}

uint32_t BitReader::read_ue() {
  int leading_zeros = 0;
  while (true) {
    if (bit_pos_ >= rbsp_.size() * 8) {
      fail_parse("truncated ue(v) codeword");
    }
    if (read_bit() == 1) {
      break;
    }
    if (++leading_zeros > 31) {
      fail_parse("illegal ue(v): more than 31 leading zeros");
    }
  }
  last_ue_bits_ = 2 * leading_zeros + 1;
  if (leading_zeros == 0) {
    return 0;
  }
  uint32_t suffix = read_bits(leading_zeros);
  return ((1u << leading_zeros) - 1u) + suffix;
}

int32_t BitReader::read_se() {
  uint32_t k = read_ue();
  if (k & 1u) {
    return static_cast<int32_t>((k + 1u) / 2u);
  }
  return -static_cast<int32_t>(k / 2u);
}

uint32_t BitReader::read_te(uint32_t range) {
  if (range > 1) {
    return read_ue();
  }
  last_ue_bits_ = 1;
  return read_bit() ^ 1u;
}

bool BitReader::more_rbsp_data() const {
  size_t total = rbsp_.size() * 8;
  if (bit_pos_ >= total) {
    return false;
  }
  size_t first_one = total;
  for (size_t i = bit_pos_; i < total; ++i) {
    if ((rbsp_[i / 8] >> (7 - (i % 8))) & 1u) {
      first_one = i;
      break;
    }
  }
  if (first_one == total) {
    return false;
  }
  for (size_t i = first_one + 1; i < total; ++i) {
    if ((rbsp_[i / 8] >> (7 - (i % 8))) & 1u) {
      return true;
    }
  }
  return false;
}

}  // namespace h264
