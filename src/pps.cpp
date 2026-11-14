#include "pps.h"

namespace h264 {

Pps parse_pps(const uint8_t* data, size_t size, const SpsStore& sps) {
  BitReader br(data, size);
  Pps p;
  p.pps_id = br.read_ue();
  p.sps_id = br.read_ue();
  if (!sps.find(p.sps_id)) {
    fail_parse("PPS " + std::to_string(p.pps_id) + " references missing SPS " +
               std::to_string(p.sps_id));
  }
  p.entropy_coding_mode_flag = br.read_bit() != 0;
  p.bottom_field_pic_order_in_frame_present_flag = br.read_bit() != 0;
  p.num_slice_groups_minus1 = br.read_ue();
  if (p.num_slice_groups_minus1 > 0) {
    fail_unsupported("slice groups (FMO) not supported");
  }
  br.read_ue();  // num_ref_idx_l0_default_active_minus1
  br.read_ue();  // num_ref_idx_l1_default_active_minus1
  br.read_bit(); // weighted_pred_flag
  br.read_bits(2);  // weighted_bipred_idc
  br.read_se();  // pic_init_qp_minus26
  br.read_se();  // pic_init_qs_minus26
  br.read_se();  // chroma_qp_index_offset
  br.read_bit();  // deblocking_filter_control_present_flag
  br.read_bit();  // constrained_intra_pred_flag
  p.redundant_pic_cnt_present_flag = br.read_bit() != 0;
  return p;
}

const Pps* PpsStore::find(uint32_t id) const {
  auto it = map_.find(id);
  return it == map_.end() ? nullptr : &it->second;
}

}  // namespace h264
