#pragma once

#include <cstdint>

#include "nal.h"
#include "pps.h"
#include "sps.h"

namespace h264 {

enum class FrameType { kI, kP, kB, kOther };

// Normalizes slice_type 0..9: {2,5,7}->I, {0}->P, {1,6}->B, rest->Other.
FrameType normalize_slice_type(uint32_t slice_type);
const char* frame_type_name(FrameType t);

struct SliceHeader {
  uint32_t first_mb_in_slice = 0;
  uint32_t slice_type = 0;
  uint32_t pps_id = 0;
  uint32_t frame_num = 0;
  bool field_pic_flag = false;
  bool bottom_field_flag = false;
  uint32_t idr_pic_id = 0;
  int64_t pic_order_cnt_lsb = 0;
  bool idr = false;
  FrameType frame_type = FrameType::kOther;
  const Pps* pps = nullptr;
  const Sps* sps = nullptr;
  size_t header_bits = 0;     // bits consumed by the parsed header prefix
  uint64_t ue_count = 0;      // ue/se/te codewords in the header
  uint64_t ue_bits = 0;       // total bits spent on those codewords
};

// Parses the leading slice-header fields from a slice NAL payload
// (EBSP, header byte excluded). Resolves pps_id against the active stores;
// a missing PPS or SPS reference is a kParse error.
SliceHeader parse_slice_header(const uint8_t* data, size_t size, bool idr,
                               const PpsStore& pps_store, const SpsStore& sps_store);

}  // namespace h264
