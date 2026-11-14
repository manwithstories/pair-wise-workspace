#pragma once

#include <cstdint>

namespace h264 {

struct NalHeader {
  uint8_t nal_ref_idc = 0;
  uint8_t nal_unit_type = 0;
};

// Parses the 1-byte NAL header. Throws kParse when forbidden_zero_bit is set.
NalHeader parse_nal_header(uint8_t byte);

const char* nal_type_name(uint8_t nal_unit_type);

// SVC (14, 15) and MVC (20) extension NALs are out of scope.
bool nal_type_unsupported(uint8_t nal_unit_type);

// Slice-derived fields used for access-unit boundary decisions.
struct AuSliceInfo {
  uint32_t first_mb_in_slice = 0;
  uint32_t frame_num = 0;
  uint32_t pps_id = 0;
  uint32_t idr_pic_id = 0;
  bool field_pic = false;
  bool bottom_field = false;
  bool idr = false;
  int64_t poc_lsb = 0;
};

// A slice with first_mb_in_slice == 0 always opens a new access unit.
bool is_new_access_unit(bool have_prev, const AuSliceInfo& cur);

// True when two slices carry identical picture-identifying fields.
bool same_picture_params(const AuSliceInfo& a, const AuSliceInfo& b);

}  // namespace h264
