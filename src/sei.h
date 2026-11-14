#pragma once

#include <cstdint>

#include "sps.h"

namespace h264 {

struct SeiRecoveryPoint {
  uint32_t recovery_frame_cnt = 0;
  bool exact_match_flag = false;
  bool broken_link_flag = false;
};

struct SeiInfo {
  bool has_recovery_point = false;
  SeiRecoveryPoint recovery_point;
  bool has_buffering_period = false;
  uint32_t buffering_period_sps_id = 0;
};

// Parses one SEI NAL payload (EBSP, header byte excluded), walking all
// buffered messages. Recovery point and buffering period payloads are
// decoded; other payload types are skipped by length.
SeiInfo parse_sei(const uint8_t* data, size_t size, const SpsStore& sps);

}  // namespace h264
