#pragma once
// SPS/PPS 解析：输入扫描器产出的 EBSP（含 NAL 头），内部完成 RBSP
// 防竞争字节移除后用 BitReader + ExpGolomb 解码。失败不抛异常，
// 返回 ok=false 并给出原因，供 strict/tolerant 两种模式统一处理。
#include <cstdint>
#include <string>
#include <vector>

#include "exp_golomb.h"
#include "nal_scanner.h"

namespace h264 {

struct SpsInfo {
  uint8_t profile_idc = 0;
  uint8_t constraint_flags = 0;
  uint8_t level_idc = 0;
  uint32_t sps_id = 0;

  // High profile（100/110/122/244/44/83/86/118/128）扩展字段
  uint32_t chroma_format_idc = 1;
  uint32_t bit_depth_luma_minus8 = 0;
  uint32_t bit_depth_chroma_minus8 = 0;

  uint32_t log2_max_frame_num_minus4 = 0;
  uint32_t pic_order_cnt_type = 0;
  uint32_t log2_max_pic_order_cnt_lsb_minus4 = 0;
  uint32_t max_num_ref_frames = 0;
  bool gaps_in_frame_num_allowed = false;

  uint32_t pic_width_in_mbs_minus1 = 0;
  uint32_t pic_height_in_map_units_minus1 = 0;
  bool frame_mbs_only_flag = true;
  bool mb_adaptive_frame_field_flag = false;
  bool direct_8x8_inference_flag = false;
  bool frame_cropping_flag = false;
  uint32_t frame_crop_left = 0, frame_crop_right = 0;
  uint32_t frame_crop_top = 0, frame_crop_bottom = 0;

  // VUI 时序（可选，缺失时 valid=false）
  bool timing_valid = false;
  uint32_t num_units_in_tick = 0;
  uint32_t time_scale = 0;
  bool fixed_frame_rate = false;

  // 派生量
  uint32_t coded_width = 0;   // 裁剪后像素宽
  uint32_t coded_height = 0;  // 裁剪后像素高（场编码时已含倍高）
  uint32_t max_frame_num = 0; // 2^(log2_max_frame_num_minus4+4)
  double fps = 0.0;           // 无 VUI 时为 0
};

struct PpsInfo {
  uint32_t pps_id = 0;
  uint32_t sps_id = 0;
  bool entropy_coding_mode = false;       // CABAC
  bool bottom_field_pic_order_in_frame = false;
  uint32_t num_slice_groups_minus1 = 0;
  uint32_t num_ref_idx_l0_default = 0;
  uint32_t num_ref_idx_l1_default = 0;
  bool weighted_pred = false;
  uint8_t weighted_bipred_idc = 0;
  int32_t pic_init_qp_minus26 = 0;
  bool deblocking_filter_control = true;
  bool redundant_pic_cnt_present = false;
};

struct ParseStatus {
  bool ok = false;
  std::string error; // 失败原因（ok=false 时有值）
  GolombStats golomb; // 本次解析的 Exp-Golomb 调用统计
};

class SpsParser {
public:
  static ParseStatus parse(const NalUnit &nal, SpsInfo &out);
};

class PpsParser {
public:
  static ParseStatus parse(const NalUnit &nal, PpsInfo &out);
};

} // namespace h264
