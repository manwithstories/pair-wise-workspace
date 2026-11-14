#include "sei.h"

namespace h264 {

namespace {

uint32_t read_ff_coded(BitReader& br) {
  uint32_t v = 0;
  while (true) {
    uint32_t b = br.read_bits(8);
    v += b;
    if (b != 0xFF) {
      return v;
    }
  }
}

void skip_bits(BitReader& br, size_t n) {
  while (n > 0) {
    int step = n > 32 ? 32 : static_cast<int>(n);
    br.read_bits(step);
    n -= static_cast<size_t>(step);
  }
}

void parse_buffering_period(BitReader& br, const SpsStore& sps, SeiInfo& out) {
  uint32_t sps_id = br.read_ue();
  out.has_buffering_period = true;
  out.buffering_period_sps_id = sps_id;
  const Sps* s = sps.find(sps_id);
  if (!s) {
    fail_parse("buffering period SEI references missing SPS " + std::to_string(sps_id));
  }
  const HrdParams* hrds[2] = {&s->vui.nal_hrd, &s->vui.vcl_hrd};
  for (const HrdParams* hrd : hrds) {
    if (!hrd->present) {
      continue;
    }
    for (uint32_t i = 0; i <= hrd->cpb_cnt_minus1; ++i) {
      br.read_bits(static_cast<int>(hrd->initial_cpb_removal_delay_length_minus1 + 1));
      br.read_bits(static_cast<int>(hrd->initial_cpb_removal_delay_length_minus1 + 1));
    }
  }
}

}  // namespace

SeiInfo parse_sei(const uint8_t* data, size_t size, const SpsStore& sps) {
  BitReader br(data, size);
  SeiInfo out;
  while (br.more_rbsp_data()) {
    uint32_t payload_type = read_ff_coded(br);
    uint32_t payload_size = read_ff_coded(br);
    size_t start_bit = br.bit_position();
    if (payload_type == 1) {
      out.has_recovery_point = true;
      out.recovery_point.recovery_frame_cnt = br.read_ue();
      out.recovery_point.exact_match_flag = br.read_bit() != 0;
      out.recovery_point.broken_link_flag = br.read_bit() != 0;
      br.read_bits(2);  // changing_slice_group_idc
    } else if (payload_type == 0) {
      parse_buffering_period(br, sps, out);
    }
    size_t consumed = br.bit_position() - start_bit;
    size_t total = static_cast<size_t>(payload_size) * 8;
    if (consumed > total) {
      fail_parse("SEI payload overrun");
    }
    skip_bits(br, total - consumed);
  }
  return out;
}

}  // namespace h264
