#include "nal.h"

#include "bitreader.h"

namespace h264 {

NalHeader parse_nal_header(uint8_t byte) {
  if (byte & 0x80) {
    fail_parse("NAL header forbidden_zero_bit is set");
  }
  NalHeader h;
  h.nal_ref_idc = (byte >> 5) & 0x03;
  h.nal_unit_type = byte & 0x1F;
  return h;
}

const char* nal_type_name(uint8_t t) {
  switch (t) {
    case 1: return "slice_non_idr";
    case 2: return "slice_dpa";
    case 5: return "slice_idr";
    case 6: return "sei";
    case 7: return "sps";
    case 8: return "pps";
    case 9: return "aud";
    case 10: return "end_of_seq";
    case 11: return "end_of_stream";
    default: return "other";
  }
}

bool nal_type_unsupported(uint8_t t) { return t == 14 || t == 15 || t == 20; }

bool is_new_access_unit(bool have_prev, const AuSliceInfo& cur) {
  if (!have_prev) {
    return true;
  }
  return cur.first_mb_in_slice == 0;
}

bool same_picture_params(const AuSliceInfo& a, const AuSliceInfo& b) {
  return a.frame_num == b.frame_num && a.pps_id == b.pps_id &&
         a.field_pic == b.field_pic && a.bottom_field == b.bottom_field &&
         a.idr == b.idr && a.poc_lsb == b.poc_lsb;
}

}  // namespace h264
