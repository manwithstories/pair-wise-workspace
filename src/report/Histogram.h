#pragma once
#include <array>
#include <cstdint>
#include "image/FrameIO.h"

namespace lg {

struct ChannelStats {
  std::array<std::uint64_t, 256> buckets{};
  double mean = 0.0;
};

struct FrameStats {
  ChannelStats ch[3];
};

FrameStats computeStats(const Frame& f);

struct CompareResult {
  double maxDev[3] = {0.0, 0.0, 0.0};
  double rmse = 0.0;
  double psnrDb = 0.0; // infinity when frames are identical
};

// Per-channel max absolute deviation plus all-channel RMSE/PSNR (peak 1.0).
CompareResult compareFrames(const Frame& a, const Frame& b);

} // namespace lg
