#pragma once
// GOP 分析器：把逐 NAL 的 slice 解析结果按图像聚合，检测随机访问点，
// 划分图像组并计算每组的起始 NAL 绝对偏移。不持有整个码流，只维护
// 与图像数量同阶的轻量结构。
#include <cstdint>
#include <string>
#include <vector>

#include "nal_scanner.h"
#include "slice_parser.h"
#include "sps_parser.h"

namespace h264 {

struct FieldPairInfo {
  uint32_t frame_num = 0;
  bool top_present = false;
  bool bottom_present = false;
  bool complete = false; // 顶底两场都在
};

struct PictureInfo {
  uint32_t frame_num = 0;
  uint8_t slice_type = SLICE_UNKNOWN; // 以首个分片为准
  bool is_i = false, is_p = false, is_b = false;
  bool idr = false;
  bool field_pic = false;
  bool bottom_field = false;
  uint32_t slice_count = 0;
  uint32_t first_mb_min = 0;
  uint32_t first_mb_max = 0;
  uint64_t first_nal_offset = 0; // 该图像首个 NAL 的起始码偏移
  uint64_t last_nal_offset = 0;
  size_t gop_index = 0;
};

struct GopInfo {
  uint32_t gop_id = 0;
  uint64_t start_offset = 0;     // 组首 NAL 的绝对起始偏移
  uint8_t start_slice_type = SLICE_UNKNOWN;
  bool starts_with_idr = false;
  uint32_t picture_count = 0;
  uint32_t i_count = 0;
  uint32_t p_count = 0;
  uint32_t b_count = 0;
};

struct AnalyzerWarning {
  uint64_t nal_offset = 0;
  std::string reason;
};

struct AnalysisResult {
  std::vector<SpsInfo> sps;
  std::vector<PpsInfo> pps;
  std::vector<PictureInfo> pictures;
  std::vector<GopInfo> gops;
  std::vector<FieldPairInfo> field_pairs;
  std::vector<AnalyzerWarning> warnings;
  uint32_t nal_count = 0;
  uint32_t vcl_nal_count = 0;
  uint32_t truncated_nal_count = 0;
  uint32_t parse_failure_count = 0;
  uint32_t slice_count = 0;          // 所有 VCL 分片总数
  uint32_t field_pair_complete = 0;
  uint32_t field_pair_incomplete = 0;
  uint32_t min_slices_per_pic = 0;
  uint32_t max_slices_per_pic = 0;
};

class GopAnalyzer {
public:
  explicit GopAnalyzer(bool strict = false) : strict_(strict) {}

  // 逐 NAL 喂入；返回 false 表示 strict 模式下遇到不可恢复错误
  bool processNal(const NalUnit &nal);
  AnalysisResult finish();

private:
  void openGop(const PictureInfo &pic);
  PictureInfo *findOrCreatePicture(const SliceInfo &sl, const NalUnit &nal,
                                   bool idr);
  static bool samePicture(const PictureInfo &pic, const SliceInfo &sl,
                          bool field, bool bottom);

  bool strict_;
  AnalysisResult r_;
  SpsInfo active_sps_;
  bool has_sps_ = false;
  PpsInfo active_pps_;
  bool has_pps_ = false;
  uint32_t cur_gop_ = 0;
};

} // namespace h264
