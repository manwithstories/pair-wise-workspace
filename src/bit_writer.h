#pragma once
// BitWriter：MSB 优先的位流写入器，供夹具生成与解析器单元测试使用。
// 与 BitReader 的位序严格对应；支持 ue(v)/se(v) 编码与 RBSP 收尾。
#include <cstdint>
#include <vector>

namespace h264 {

class BitWriter {
public:
  void writeBits(uint32_t value, unsigned n);
  void writeBit(bool one) { writeBits(one ? 1u : 0u, 1); }
  void writeUE(uint32_t code_num);
  void writeSE(int32_t signed_value);

  // RBSP 收尾：写 1 个 stop bit(1)，再补 0 到字节对齐
  void writeRbspTrailingBits();

  // 插入防竞争字节，输出 Annex B 可用的 EBSP
  static std::vector<uint8_t> addEmulationBytes(const std::vector<uint8_t> &rbsp);

  const std::vector<uint8_t> &bytes() const { return buf_; }
  size_t bitCount() const { return bit_pos_; }
  void reset() { buf_.clear(); bit_pos_ = 0; }

private:
  std::vector<uint8_t> buf_;
  size_t bit_pos_ = 0;
};

} // namespace h264
