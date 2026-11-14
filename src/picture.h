#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <vector>

#include "slice.h"

namespace h264 {

struct GopInfo {
  uint64_t index = 0;
  uint64_t start_byte_offset = 0;
  uint64_t au_index = 0;
  uint64_t frame_count = 0;
  std::map<std::string, uint64_t> type_mix;
  int64_t poc_start = 0;
};

struct StreamStats {
  uint64_t count_i = 0, count_p = 0, count_b = 0, count_other = 0;
  uint64_t total_frames() const { return count_i + count_p + count_b + count_other; }
  std::vector<GopInfo> gops;
  std::map<uint32_t, uint64_t> slices_per_picture;
  uint64_t ue_count = 0;
  uint64_t ue_bits = 0;
  uint64_t header_bits = 0;
  std::vector<std::string> warnings;
  uint64_t warning_overflow = 0;
};

// Merges slices into pictures (access units), pairs complementary fields
// into frames, classifies frame types and cuts GOPs at IDR pictures or
// recovery-point SEI messages. Memory use is O(gops + warnings).
class PictureAggregator {
public:
  void on_slice(const SliceHeader& h, uint8_t nal_ref_idc, uint64_t nal_offset);
  void on_recovery_point() { recovery_pending_ = true; }
  void finish();

  const StreamStats& stats() const { return st_; }

private:
  struct PendingField {
    FrameType type;
    uint64_t offset, au_index;
    uint32_t frame_num;
    bool bottom, idr;
    int64_t poc;
  };

  void finalize_picture();
  void emit_frame(FrameType type, uint64_t offset, uint64_t au_index, int64_t poc, bool idr);
  int64_t compute_poc(const SliceHeader& h, uint8_t nal_ref_idc);
  void warn(const std::string& msg);

  StreamStats st_;
  bool have_pic_ = false;
  uint64_t pic_au_index_ = 0, pic_offset_ = 0;
  FrameType pic_type_ = FrameType::kOther;
  bool pic_field_ = false, pic_bottom_ = false, pic_idr_ = false;
  uint32_t pic_frame_num_ = 0, pic_pps_id_ = 0, pic_slices_ = 0;
  uint32_t last_first_mb_ = 0;
  int64_t pic_poc_ = 0;
  const Sps* pic_sps_ = nullptr;
  uint64_t au_counter_ = 0;
  uint32_t prev_pic_slices_ = 0;
  bool have_pending_field_ = false;
  PendingField pending_{};
  bool recovery_pending_ = false;
  bool gop_open_ = false;
  GopInfo cur_gop_;
  int64_t prev_poc_lsb_ = 0, prev_poc_msb_ = 0;
};

}  // namespace h264
