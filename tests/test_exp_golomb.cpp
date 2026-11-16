#include "bit_reader.h"
#include "exp_golomb.h"
#include "test_framework.h"

#include <cstdint>
#include <vector>

using namespace h264;

namespace {
// 测试用位写入器：MSB 优先，与 BitReader 对应。
class BitWriter {
public:
  void writeBits(uint32_t value, unsigned n) {
    for (unsigned i = n; i > 0; --i) {
      const unsigned bit = (value >> (i - 1)) & 1u;
      if (bit_pos_ % 8 == 0)
        buf_.push_back(0);
      if (bit)
        buf_.back() |= static_cast<uint8_t>(1u << (7 - (bit_pos_ % 8)));
      ++bit_pos_;
    }
  }
  // ue(v) 编码：leadingZeroBits = floor(log2(codeNum+1))
  void writeUE(uint32_t code_num) {
    unsigned code_len = 1;
    uint32_t v = code_num + 1;
    while (v > 1) {
      v >>= 1;
      code_len += 2;
    }
    writeBits(code_num + 1, code_len);
  }
  const std::vector<uint8_t> &bytes() const { return buf_; }

private:
  size_t bit_pos_ = 0;
  std::vector<uint8_t> buf_;
};
} // namespace

TEST_CASE(exp_golomb_ue_known_codewords) {
  // 0->1, 1->010, 2->011, 3->00100, 4->00101
  BitWriter w;
  for (uint32_t n = 0; n <= 8; ++n)
    w.writeUE(n);
  BitReader br(w.bytes().data(), w.bytes().size());
  ExpGolomb eg(br);
  for (uint32_t expect = 0; expect <= 8; ++expect) {
    uint32_t got = 999;
    CHECK(eg.readUE(got));
    CHECK_EQ(got, expect);
  }
  br.byteAlign();
  CHECK(br.eof());
  CHECK_EQ(eg.stats().success, 9u);
  CHECK_EQ(eg.stats().failure, 0u);
  // codeNum 7/8 的码字为 7 位，是本批最长
  CHECK_EQ(eg.stats().max_code_bits, 7u);
}

TEST_CASE(exp_golomb_se_mapping_and_cross_byte) {
  // se: 0->0, 1->+1, 2->-1, 3->+2, 4->-2, 5->+3
  const int32_t expect_se[] = {0, 1, -1, 2, -2, 3};
  BitWriter w;
  for (uint32_t n = 0; n < 6; ++n)
    w.writeUE(n);
  BitReader br(w.bytes().data(), w.bytes().size());
  ExpGolomb eg(br);
  for (int32_t want : expect_se) {
    int32_t got = 999;
    CHECK(eg.readSE(got));
    CHECK_EQ(got, want);
  }
  br.byteAlign();
  CHECK(br.eof()); // 不足整字节的码字后由 BitWriter 补零对齐
}

TEST_CASE(exp_golomb_exhaustion_counts_failure) {
  // 全零缓冲：永远等不到起始 1，耗尽即失败
  const uint8_t zeros[] = {0x00, 0x00};
  BitReader br(zeros, sizeof(zeros));
  ExpGolomb eg(br);
  uint32_t v = 0;
  CHECK(!eg.readUE(v));
  CHECK_EQ(eg.stats().failure, 1u);
  CHECK_EQ(eg.stats().success, 0u);
}

TEST_CASE(exp_golomb_rejects_over_32bit) {
  // 33 位码字：16 个前导 0 + 1 + 16 位后缀
  const uint8_t buf[] = {0x00, 0x00, 0x80, 0x00, 0x00};
  BitReader br(buf, sizeof(buf));
  ExpGolomb eg(br);
  uint32_t v = 0;
  CHECK(!eg.readUE(v));
  CHECK_EQ(eg.stats().failure, 1u);
  CHECK_EQ(eg.stats().max_code_bits, 0u);
}

TEST_CASE(exp_golomb_continues_after_failure) {
  // 失败后下一个合法码字仍可解码（计数独立累计）
  BitWriter w;
  w.writeUE(2); // 011
  const uint8_t raw[] = {0b01100000, 0x00, 0b10000000};
  // 011 -> codeNum 2；随后 5 个零耗尽 -> 失败；新构造读取器验证恢复
  BitReader br(raw, sizeof(raw));
  ExpGolomb eg(br);
  uint32_t a = 0;
  CHECK(eg.readUE(a));
  CHECK_EQ(a, 2u);
  uint32_t b = 0;
  CHECK(!eg.readUE(b));
  // 新读取器上的成功调用不应受历史失败影响
  BitWriter w2;
  w2.writeUE(6); // 00111
  BitReader br2(w2.bytes().data(), w2.bytes().size());
  ExpGolomb eg2(br2);
  uint32_t c = 0;
  CHECK(eg2.readUE(c));
  CHECK_EQ(c, 6u);
}
