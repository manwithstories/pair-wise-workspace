#pragma once
// NAL 扫描器：流式识别 Annex B 起始码（00 00 01 / 00 00 00 01），
// 记录每个 NAL 的绝对字节偏移。支持分块喂入，内存只保留当前 NAL。
#include <cstddef>
#include <cstdint>
#include <iosfwd>
#include <vector>

namespace h264 {

// H.264 Table 7-1 NAL unit type 常量
enum NalUnitType : uint8_t {
  NAL_SLICE = 1,        // 非 IDR 编码条带
  NAL_DPA = 2,
  NAL_DPB = 3,
  NAL_DPC = 4,
  NAL_IDR_SLICE = 5,
  NAL_SEI = 6,
  NAL_SPS = 7,
  NAL_PPS = 8,
  NAL_AUD = 9,
  NAL_END_SEQUENCE = 10,
  NAL_END_STREAM = 11,
};

struct NalUnit {
  uint64_t start_offset = 0;    // 起始码首字节的绝对偏移
  uint32_t start_code_len = 0;  // 3 或 4
  uint64_t header_offset = 0;   // NAL 头字节偏移 = start_offset + len
  uint8_t forbidden_zero_bit = 0;
  uint8_t nal_ref_idc = 0;
  uint8_t nal_unit_type = 0;
  std::vector<uint8_t> ebsp;    // 含 NAL 头的原始 EBSP（未去防竞争字节）
  bool truncated = false;       // 末尾未见下一起始码/EOF 的不完整 NAL
};

class NalScanner {
public:
  // 分块喂入；每发现一个完整 NAL 即追加到 out。
  void feed(const uint8_t *data, size_t size, std::vector<NalUnit> &out);
  // 流结束调用：残留的不完整 NAL 以 truncated=true 输出（空负载不输出）。
  void finish(std::vector<NalUnit> &out);

  // 整缓冲便捷接口
  static std::vector<NalUnit> scanAll(const uint8_t *data, size_t size);
  // 流式文件接口：按 chunk_size 分块读取，适合大文件
  static std::vector<NalUnit> scanStream(std::istream &in,
                                         size_t chunk_size = 65536);

  static bool isVclNal(uint8_t nal_unit_type) {
    return nal_unit_type >= 1 && nal_unit_type <= 5;
  }

private:
  void emitCurrent(std::vector<NalUnit> &out, bool truncated);

  bool in_nal_ = false;
  unsigned zero_run_ = 0;
  uint64_t cur_start_ = 0;
  uint32_t cur_sc_len_ = 0;
  uint64_t total_pos_ = 0;
  std::vector<uint8_t> payload_;
};

} // namespace h264
