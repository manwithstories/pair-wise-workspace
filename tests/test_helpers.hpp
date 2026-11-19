#pragma once
// 测试公共辅助与统一头文件。
#include <catch2/catch_approx.hpp>
#include <catch2/catch_test_macros.hpp>
#include <filesystem>
#include <fstream>
#include <unistd.h>
#include "media/clock_repair.hpp"
#include "media/manifest.hpp"
#include "media/packet.hpp"
#include "media/stream_pipeline.hpp"
#include "media/time_model.hpp"

using namespace media;
using Catch::Approx;

namespace media_test {
inline MediaPacket mkPkt(size_t gi, int64_t pts, int64_t dur,
                         bool key = false) {
  MediaPacket p;
  p.globalIndex = gi;
  p.index = gi;
  p.rawPts = pts;
  p.rawDts = pts;
  p.durationTicks = dur;
  p.keyframe = key;
  return p;
}
} // namespace media_test
