#pragma once

#include <cstdint>
#include <map>

#include "bitreader.h"
#include "sps.h"

namespace h264 {

struct Pps {
  uint32_t pps_id = 0;
  uint32_t sps_id = 0;
  bool entropy_coding_mode_flag = false;
  bool bottom_field_pic_order_in_frame_present_flag = false;
  uint32_t num_slice_groups_minus1 = 0;
  bool redundant_pic_cnt_present_flag = false;
};

// Parses one PPS NAL payload (EBSP, header byte excluded).
// Throws kParse when the referenced SPS id is not yet active.
Pps parse_pps(const uint8_t* data, size_t size, const SpsStore& sps);

class PpsStore {
public:
  void add(Pps pps) { map_[pps.pps_id] = pps; }
  const Pps* find(uint32_t id) const;
  size_t size() const { return map_.size(); }

private:
  std::map<uint32_t, Pps> map_;
};

}  // namespace h264
