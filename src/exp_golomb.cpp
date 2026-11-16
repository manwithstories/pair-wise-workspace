#include "exp_golomb.h"

namespace h264 {

bool ExpGolomb::readUE(uint32_t &code_num) {
  const size_t start = br_.bitPos();
  unsigned leading_zeros = 0;
  try {
    // 码字位长 = 2*leadingZeroBits + 1；超过 32 位意味着
    // leadingZeroBits >= 16（最短 33 位），按需求拒绝。
    while (true) {
      if (leading_zeros > 15) {
        ++stats_.failure;
        return false;
      }
      if (br_.readBit())
        break;
      ++leading_zeros;
    }
    uint32_t suffix = 0;
    if (leading_zeros > 0)
      suffix = br_.readBits(leading_zeros);
    // leading_zeros <= 15 时 2^16-1+65535 = 131071，32 位无溢出风险。
    code_num = (1u << leading_zeros) - 1u + suffix;

    const unsigned code_bits =
        static_cast<unsigned>(br_.bitPos() - start);
    if (code_bits > stats_.max_code_bits)
      stats_.max_code_bits = code_bits;
    ++stats_.success;
    return true;
  } catch (const BitReaderError &) {
    ++stats_.failure;
    return false;
  }
}

bool ExpGolomb::readSE(int32_t &signed_value) {
  uint32_t code_num = 0;
  if (!readUE(code_num))
    return false; // readUE 已计 failure
  // se(v): codeNum 偶数 -> -codeNum/2；奇数 -> (codeNum+1)/2
  if ((code_num & 1u) != 0)
    signed_value = static_cast<int32_t>((code_num >> 1) + 1u);
  else
    signed_value = -static_cast<int32_t>(code_num >> 1);
  return true;
}

} // namespace h264
