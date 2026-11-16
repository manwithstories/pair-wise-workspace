#include "nal_scanner.h"
#include "test_framework.h"

#include <algorithm>
#include <sstream>
#include <string>
#include <vector>

using namespace h264;

namespace {
// 手工拼装 Annex B 流的小工具
class StreamBuilder {
public:
  void nal(uint8_t header, const std::vector<uint8_t> &payload,
           bool four_byte = false) {
    buf_.push_back(0x00);
    buf_.push_back(0x00);
    if (four_byte)
      buf_.push_back(0x00);
    buf_.push_back(0x01);
    buf_.push_back(header);
    for (uint8_t b : payload)
      buf_.push_back(b);
  }
  void raw(const std::vector<uint8_t> &bytes) {
    for (uint8_t b : bytes)
      buf_.push_back(b);
  }
  const std::vector<uint8_t> &bytes() const { return buf_; }

private:
  std::vector<uint8_t> buf_;
};
} // namespace

TEST_CASE(scanner_basic_offsets_and_types) {
  StreamBuilder b;
  // 负载以非零字节结尾，避免与起始码零产生歧义（真实 EBSP 由防竞争字节保证）
  b.nal(0x67, {0x42, 0x00, 0x00, 0x03}); // SPS, nal_ref_idc=3
  b.nal(0x65, {0x88, 0x80});             // IDR
  auto nals = NalScanner::scanAll(b.bytes().data(), b.bytes().size());
  CHECK_EQ(nals.size(), 2u);

  CHECK_EQ(nals[0].start_offset, 0u);
  CHECK_EQ(nals[0].start_code_len, 3u);
  CHECK_EQ(nals[0].header_offset, 3u);
  CHECK_EQ(nals[0].nal_unit_type, (unsigned)NAL_SPS);
  CHECK_EQ(nals[0].nal_ref_idc, 3u);
  CHECK_EQ(nals[0].forbidden_zero_bit, 0u);
  CHECK(!nals[0].truncated);
  const std::vector<uint8_t> p0 = {0x67, 0x42, 0x00, 0x00, 0x03};
  CHECK(nals[0].ebsp == p0);

  // 第二个 NAL 起始偏移 = 3 + 5（头+4 负载）
  CHECK_EQ(nals[1].start_offset, 8u);
  CHECK_EQ(nals[1].nal_unit_type, (unsigned)NAL_IDR_SLICE);
}

TEST_CASE(scanner_four_byte_and_consecutive_start_codes) {
  // 手工构造：4 字节起始码 NAL，紧接一个空 NAL（连续起始码），再一个正常 NAL
  StreamBuilder b;
  b.nal(0x67, {0xAA}, true); // 4 字节起始码
  b.raw({0x00, 0x00, 0x01}); // 与下一起始码相邻 -> 空 NAL，应被丢弃
  b.nal(0x41, {0x01});       // P slice
  auto nals = NalScanner::scanAll(b.bytes().data(), b.bytes().size());
  CHECK_EQ(nals.size(), 2u); // 空 NAL 不计数
  CHECK_EQ(nals[0].start_code_len, 4u);
  CHECK_EQ(nals[0].start_offset, 0u);
  CHECK_EQ(nals[1].start_code_len, 3u);
  CHECK_EQ(nals[1].nal_unit_type, (unsigned)NAL_SLICE);
}

TEST_CASE(scanner_zeros_inside_payload_preserved) {
  // 负载内部的 00 00（后跟非 01）必须原样保留
  StreamBuilder b;
  b.nal(0x67, {0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x05});
  auto nals = NalScanner::scanAll(b.bytes().data(), b.bytes().size());
  CHECK_EQ(nals.size(), 1u);
  const std::vector<uint8_t> expect = {0x67, 0x00, 0x00, 0x02,
                                       0x00, 0x00, 0x00, 0x05};
  CHECK(nals[0].ebsp == expect);
}

TEST_CASE(scanner_truncated_tail_flagged) {
  StreamBuilder b;
  b.nal(0x67, {0x42});
  b.raw({0x00, 0x00, 0x01, 0x65, 0x11, 0x22}); // 末尾 NAL 无后续起始码/EOF
  auto nals = NalScanner::scanAll(b.bytes().data(), b.bytes().size());
  CHECK_EQ(nals.size(), 2u);
  CHECK(!nals[0].truncated);
  CHECK(nals[1].truncated);
  CHECK_EQ(nals[1].nal_unit_type, (unsigned)NAL_IDR_SLICE);
}

TEST_CASE(scanner_chunked_feeds_identical_result) {
  StreamBuilder b;
  for (int i = 0; i < 10; ++i)
    b.nal(static_cast<uint8_t>(0x60 + (i % 8)),
          {static_cast<uint8_t>(i), 0x00, 0x00, 0x03});
  const auto &full = b.bytes();

  NalScanner s;
  std::vector<NalUnit> got;
  // 按各种小块大小喂入，起始码会被任意切断
  for (size_t off = 0; off < full.size();) {
    size_t n = std::min<size_t>(3, full.size() - off);
    s.feed(full.data() + off, n, got);
    off += n;
  }
  s.finish(got);

  auto expect = NalScanner::scanAll(full.data(), full.size());
  CHECK_EQ(got.size(), expect.size());
  for (size_t i = 0; i < got.size(); ++i) {
    CHECK_EQ(got[i].start_offset, expect[i].start_offset);
    CHECK_EQ(got[i].start_code_len, expect[i].start_code_len);
    CHECK(got[i].ebsp == expect[i].ebsp);
    CHECK_EQ(got[i].truncated, expect[i].truncated);
  }
}

TEST_CASE(scanner_stream_and_garbage_prefix) {
  StreamBuilder b;
  b.raw({0x99, 0x88}); // 起始码前的杂散字节
  b.nal(0x67, {0x42});
  std::istringstream in(
      std::string(reinterpret_cast<const char *>(b.bytes().data()),
                  b.bytes().size()));
  auto nals = NalScanner::scanStream(in, 4); // 极小分块
  CHECK_EQ(nals.size(), 1u);
  CHECK_EQ(nals[0].start_offset, 2u);
  CHECK_EQ(nals[0].nal_unit_type, (unsigned)NAL_SPS);
}

TEST_CASE(scanner_callback_stream_matches_vector) {
  // 回调式流式扫描与整缓冲扫描结果必须逐 NAL 一致，且回调即交付不累积
  StreamBuilder b;
  for (int i = 0; i < 12; ++i)
    b.nal(static_cast<uint8_t>(0x60 + (i % 8)),
          {static_cast<uint8_t>(i), 0xAB});
  const auto &full = b.bytes();
  auto expect = NalScanner::scanAll(full.data(), full.size());

  std::istringstream in(
      std::string(reinterpret_cast<const char *>(full.data()), full.size()));
  std::vector<NalUnit> got;
  NalScanner::scanStreamCb(
      in, [&](const NalUnit &n) { got.push_back(n); }, /*chunk=*/5);

  CHECK_EQ(got.size(), expect.size());
  for (size_t i = 0; i < got.size(); ++i) {
    CHECK_EQ(got[i].start_offset, expect[i].start_offset);
    CHECK_EQ(got[i].nal_unit_type, expect[i].nal_unit_type);
    CHECK(got[i].ebsp == expect[i].ebsp);
  }
}
