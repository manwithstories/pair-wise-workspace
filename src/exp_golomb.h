#pragma once
// Exp-Golomb 解码器（H.264 9.1）：ue(v) / se(v)，包装 BitReader，
// 累计成功次数、失败次数与最大码长（位）。
#include <cstdint>

#include "bit_reader.h"

namespace h264 {

struct GolombStats {
  uint64_t success = 0;
  uint64_t failure = 0;
  unsigned max_code_bits = 0; // 实际成功解码的最大码字位长
};

class ExpGolomb {
public:
  explicit ExpGolomb(BitReader &br) : br_(br) {}

  // 失败（位流耗尽 / 码长超过 32 位）返回 false 并计数，不抛出。
  bool readUE(uint32_t &code_num);
  bool readSE(int32_t &signed_value);

  const GolombStats &stats() const { return stats_; }

private:
  BitReader &br_;
  GolombStats stats_;
};

} // namespace h264
