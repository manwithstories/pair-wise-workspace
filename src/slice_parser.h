#pragma once
// Slice header 解析：识别条带类型（I/P/B）、frame_num、场编码标记与
// first_mb_in_slice（作为 slice_id）。解析深度以结构分析所需字段为界，
// 参考帧列表/加权预测等纯解码字段在确认主字段后即停止。
#include <cstdint>
#include <string>

#include "nal_scanner.h"
#include "sps_parser.h"

namespace h264 {

enum SliceType : uint8_t {
  SLICE_P = 0,
  SLICE_B = 1,
  SLICE_I = 2,
  SLICE_SP = 3,
  SLICE_SI = 4,
  SLICE_UNKNOWN = 255,
};

struct SliceInfo {
  uint32_t first_mb_in_slice = 0; // 同时作为 slice_id
  uint8_t slice_type = SLICE_UNKNOWN;
  bool is_i = false;
  bool is_p = false;
  bool is_b = false;
  uint32_t pps_id = 0;
  uint32_t frame_num = 0;
  bool field_pic_flag = false;
  bool bottom_field_flag = false;
  uint32_t idr_pic_id = 0;       // 仅 IDR
  bool idr_pic_id_present = false;
  uint32_t pic_order_cnt_lsb = 0;
  bool delta_pic_order_valid = false;
  int32_t delta_pic_order_cnt_bottom = 0;
  bool redundant_pic_cnt_present = false;
  uint32_t redundant_pic_cnt = 0;
};

class SliceParser {
public:
  // 依赖 SPS（frame_num 位宽、场编码能力、POC 类型）与 PPS（冗余计数）
  static ParseStatus parse(const NalUnit &nal, const SpsInfo &sps,
                           const PpsInfo &pps, SliceInfo &out);
  static const char *typeName(uint8_t slice_type);
};

} // namespace h264
