#include <cassert>
#include <cstdio>
#include <cstring>
#include <cstdlib>
#include <string>
#include <vector>

#include "annexb.h"
#include "bitreader.h"
#include "diff.h"
#include "json.h"
#include "slice.h"

using namespace h264;

#define CHECK(cond)                                                    \
  do {                                                                 \
    if (!(cond)) {                                                     \
      fprintf(stderr, "CHECK failed %s:%d: %s\n", __FILE__, __LINE__, \
              #cond);                                                  \
      exit(1);                                                         \
    }                                                                  \
  } while (0)

static void test_bitreader_ue_se_te() {
  // ue: 0=>1, 1=>010, 2=>011, 5=>00110 ; se: 0,1,-1,2
  const uint8_t data[] = {0b10100110, 0b01100000, 0x00};
  BitReader br(data, sizeof(data));
  CHECK(br.read_ue() == 0);
  CHECK(br.read_ue() == 1);
  CHECK(br.read_ue() == 2);
  CHECK(br.read_ue() == 5);
  CHECK(br.last_ue_bits() == 5);
  BitReader br2(data, sizeof(data));
  CHECK(br2.read_se() == 0);
  CHECK(br2.read_se() == 1);
  CHECK(br2.read_se() == -1);
  CHECK(br2.read_se() == 3);
  const uint8_t one[] = {0x80};
  BitReader br3(one, 1);
  CHECK(br3.read_te(1) == 0);
  bool threw = false;
  try {
    const uint8_t zeros[] = {0x00, 0x00, 0x00, 0x00, 0x00};
    BitReader br4(zeros, sizeof(zeros));
    br4.read_ue();
  } catch (const Error& e) {
    threw = e.code() == ErrCode::kParse;
  }
  CHECK(threw);
}

static void test_emulation_prevention() {
  const uint8_t ebsp[] = {0x00, 0x00, 0x03, 0x01, 0xFF};
  BitReader br(ebsp, sizeof(ebsp));
  CHECK(br.read_bits(8) == 0x00);
  CHECK(br.read_bits(8) == 0x00);
  CHECK(br.read_bits(8) == 0x01);
  CHECK(br.byte_offset() == 3);
}

static void test_annexb_split() {
  // Leading garbage without start code (ignored), 4-byte start code +
  // SPS-like NAL, zero-length NAL, 3-byte start codes, trailing zeros.
  const uint8_t stream[] = {
      0x09, 0x09,
      0x00, 0x00, 0x00, 0x01, 0x67, 0x11, 0x22,
      0x00, 0x00, 0x01,
      0x00, 0x00, 0x01, 0x68, 0x33,
      0x00, 0x00, 0x00, 0x01, 0x65, 0x44, 0x00, 0x00};
  FILE* fp = tmpfile();
  fwrite(stream, 1, sizeof(stream), fp);
  rewind(fp);
  std::vector<NalUnit> nals;
  ScanStats stats;
  scan_annexb(fp, [&](const NalUnit& n) { nals.push_back(n); }, stats);
  fclose(fp);
  CHECK(nals.size() == 4);
  CHECK(nals[0].offset == 6 && nals[0].length == 3 && nals[0].nal_type == 7);
  CHECK(nals[1].length == 0 && stats.zero_length_nals == 1);
  CHECK(nals[2].offset == 15 && nals[2].length == 2 && nals[2].nal_type == 8);
  CHECK(nals[3].offset == 21 && nals[3].length == 4 && nals[3].nal_type == 5);
  CHECK(nals[3].prefix_len == 3 && nals[3].prefix[2] == 0x00);
}

static void test_slice_type_normalize() {
  CHECK(normalize_slice_type(0) == FrameType::kP);
  CHECK(normalize_slice_type(1) == FrameType::kB);
  CHECK(normalize_slice_type(2) == FrameType::kI);
  CHECK(normalize_slice_type(5) == FrameType::kI);
  CHECK(normalize_slice_type(6) == FrameType::kB);
  CHECK(normalize_slice_type(7) == FrameType::kI);
  CHECK(normalize_slice_type(3) == FrameType::kOther);
  CHECK(normalize_slice_type(9) == FrameType::kOther);
}

static void test_json_and_diff() {
  std::string a = "{\"sps\":{\"level_idc\":40},\"gops\":[{\"index\":0,\"frame_count\":8}]}";
  JsonValue v = json_parse(a);
  CHECK(v.find("sps")->find("level_idc")->number == 40);
  std::string b = "{\"sps\":{\"level_idc\":41},\"gops\":[{\"index\":0,\"frame_count\":8}]}";
  auto diffs = diff_reports(a, b);
  CHECK(diffs.size() == 1);
  CHECK(diffs[0].path == "sps.level_idc");
  CHECK(diff_reports(a, a).empty());
  std::string with_stats = "{\"file\":\"x\",\"stats\":{\"elapsed_sec\":1}}";
  CHECK(diff_reports(with_stats, with_stats).empty());
}

int main() {
  test_bitreader_ue_se_te();
  test_emulation_prevention();
  test_annexb_split();
  test_slice_type_normalize();
  test_json_and_diff();
  printf("all tests passed\n");
  return 0;
}
