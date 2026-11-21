#pragma once

#include <cstdint>

namespace media {

// 全流水线统一时间基准：整数采样刻度（sample tick），禁止浮点累计漂移。
using SampleTick = int64_t;

// 半开区间 [start, end)，单位为输入素材采样帧序号（每帧含全部声道）。
struct SampleRange {
  SampleTick start = 0;
  SampleTick end = 0;

  SampleTick length() const { return end - start; }
  bool contains(SampleTick tick) const { return tick >= start && tick < end; }
  bool valid() const { return end >= start; }
};

// 毫秒 <-> 采样刻度，全程整数运算（整除，调用方按需取整）。
constexpr SampleTick msToTicks(int64_t milliseconds, int64_t sampleRate) {
  return milliseconds * sampleRate / 1000;
}

constexpr int64_t ticksToMs(SampleTick ticks, int64_t sampleRate) {
  return ticks * 1000 / sampleRate;
}

}  // namespace media
