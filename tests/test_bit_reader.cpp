#include "bit_reader.h"
#include "test_framework.h"

#include <vector>

using namespace h264;

TEST_CASE(bit_reader_basic_and_cross_byte) {
  // 二进制: 10110010 01001111
  const uint8_t buf[] = {0xB2, 0x4F};
  BitReader br(buf, sizeof(buf));

  CHECK_EQ(br.readBits(3), 0b101u);
  CHECK_EQ(br.readBits(1), 0b1u);
  // 跨字节读取：剩余 4 位来自首字节，4 位来自第二字节
  CHECK_EQ(br.readBits(8), 0b0010'0100u);
  CHECK_EQ(br.bitPos(), 12u);
  CHECK_EQ(br.readBits(4), 0b1111u);
  CHECK(br.eof());
  CHECK_EQ(br.bitsLeft(), 0u);
}

TEST_CASE(bit_reader_byte_align) {
  const uint8_t buf[] = {0xFF, 0x0F};
  BitReader br(buf, sizeof(buf));
  br.readBits(3);
  br.byteAlign();
  CHECK_EQ(br.bitPos(), 8u);
  CHECK_EQ(br.readBits(8), 0x0Fu);
}

TEST_CASE(bit_reader_exhaustion_throws_and_try) {
  const uint8_t buf[] = {0x00};
  BitReader br(buf, 1);
  uint32_t out = 0;
  CHECK(br.tryReadBits(5, out));
  CHECK_EQ(out, 0u);
  bool caught = false;
  try {
    br.readBits(4);
  } catch (const BitReaderError &e) {
    caught = true;
    CHECK(e.code() == BitReaderError::OUT_OF_BITS);
  }
  CHECK(caught);
  CHECK(!br.tryReadBits(4, out));

  caught = false;
  try {
    br.readBits(0);
  } catch (const BitReaderError &e) {
    caught = true;
    CHECK(e.code() == BitReaderError::INVALID_ARG);
  }
  CHECK(caught);
}

TEST_CASE(bit_reader_emulation_removal) {
  // 00 00 03 00 -> 00 00 00；00 00 03 01 -> 00 00 01；普通字节保持不变
  const std::vector<uint8_t> ebsp = {0x00, 0x00, 0x03, 0x00, 0xAB,
                                     0x00, 0x00, 0x03, 0x01};
  bool had = false;
  auto rbsp = BitReader::removeEmulationBytes(ebsp.data(), ebsp.size(), had);
  CHECK(had);
  const std::vector<uint8_t> expect = {0x00, 0x00, 0x00, 0xAB,
                                       0x00, 0x00, 0x01};
  CHECK(rbsp == expect);

  // 没有防竞争字节时原样返回、had=false
  const std::vector<uint8_t> plain = {0x01, 0x02, 0x03};
  auto rbsp2 = BitReader::removeEmulationBytes(plain.data(), plain.size(), had);
  CHECK(!had);
  CHECK(rbsp2 == plain);
}

TEST_CASE(bit_reader_32bit_max_width) {
  const uint8_t buf[] = {0xDE, 0xAD, 0xBE, 0xEF};
  BitReader br(buf, 4);
  CHECK_EQ(br.readBits(32), 0xDEADBEEFu);
}
