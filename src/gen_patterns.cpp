#include "media/gen_patterns.hpp"
#include <cmath>

extern "C" {
#include <libavutil/imgutils.h>
}

namespace media::gen {
namespace {
constexpr int64_t VIDEO_TB = 90000;
constexpr int64_t AUDIO_TB = 24000;
}

std::vector<FramePlan> videoPlan(const GenConfig& cfg, uint64_t seed,
                                 double ppm, int64_t offsetUs,
                                 int corruptStride) {
  XorShift64 rng(seed);
  const int frames = cfg.seconds * 30;
  std::vector<FramePlan> plan;
  plan.reserve(frames);
  // 保证素材期内发生一次回绕：起点距 33 位边界仅约 1 秒。
  int64_t start = cfg.wrapPts ? int64_t(PTS33_MOD - VIDEO_TB) : 0;
  const double baseStepUs = 1'000'000.0 / 30.0;
  const int64_t baseStepTick = VIDEO_TB / 30; // 3000
  double driftUs = 0.0;   // 漂移累计（不含常数偏移）
  int64_t lastTick = start;
  uint64_t prevWrap = 0;
  for (int i = 0; i < frames; ++i) {
    // VFR 抖动只作用于本帧间隔（0.96..1.04），保证严格单调。
    double vfr = cfg.vfr ? 1.0 + (rng.unit() - 0.5) * 0.08 : 1.0;
    double stepUs = baseStepUs * vfr;
    driftUs += stepUs * (ppm * 1e-6);
    // 线性化时间：理想帧时间 + 纯漂移（无 VFR），用于漂移估计真值。
    double linearUs = double(i) * baseStepUs * (1.0 + ppm * 1e-6) +
                      double(offsetUs);
    int64_t linearTick = start +
        int64_t(std::llround(linearUs / 1e6 * VIDEO_TB));
    double devUs = double(i) * baseStepUs + driftUs + double(offsetUs);
    int64_t tick = start + int64_t(std::llround(devUs / 1e6 * VIDEO_TB));
    if (tick <= lastTick) tick = lastTick + 1; // 数值兜底：严格递增
    uint64_t wrapN = uint64_t(tick) / PTS33_MOD;
    int64_t pts = int64_t(uint64_t(tick) & PTS33_MASK);
    // CFR 时步长恒定；VFR 时取相邻 tick 之差，首帧用理想步长。
    int64_t dur = cfg.vfr ? (i == 0 ? baseStepTick : tick - lastTick)
                          : baseStepTick;
    plan.push_back({pts, tick, linearTick, dur, (i % 2 == 0),
                    corruptStride > 0 && (i % corruptStride == 0) && i > 0,
                    wrapN != prevWrap});
    prevWrap = wrapN;
    lastTick = tick;
  }
  return plan;
}

std::vector<AudioPlan> audioPlan(const GenConfig& cfg, int corruptStride) {
  const int64_t total = int64_t(cfg.seconds) * AUDIO_TB;
  std::vector<AudioPlan> plan;
  int64_t pts = 0;
  size_t ordinal = 0;
  while (pts < total) {
    int64_t n = std::min(int64_t(cfg.audioPacketSamples), total - pts);
    plan.push_back({pts, n,
                    corruptStride > 0 && ordinal > 0 &&
                        ordinal % size_t(corruptStride) == 0});
    pts += n;
    ++ordinal;
  }
  return plan;
}

void fillFrame(AVFrame* f, size_t index, int width, int height) {
  // Y 平面：水平渐变 + 滚动条纹 + 确定性高频噪声（提升可编码熵与码率动态）；
  // U/V：随帧缓变的平涂。噪声公式须与 decoder.cpp 的 referenceLuma 完全一致。
  for (int y = 0; y < height; ++y)
    for (int x = 0; x < width; ++x) {
      int stripe = ((x + int(index) * 4) / 16) % 2 * 24;
      int noise = int((x * 73 + y * 19 + index * 131) & 0x1F) - 16;
      f->data[0][y * f->linesize[0] + x] =
          uint8_t((x * 200 / width + y * 40 / height + stripe + noise) & 0xFF);
    }
  for (int y = 0; y < height / 2; ++y)
    for (int x = 0; x < width / 2; ++x) {
      f->data[1][y * f->linesize[1] + x] =
          uint8_t(110 + (index * 3) % 40);
      f->data[2][y * f->linesize[2] + x] =
          uint8_t(120 + (index * 5) % 30);
    }
}

void fillAudio(int16_t* dst, size_t samples, size_t packetIndex,
               int sampleRate) {
  for (size_t k = 0; k < samples; ++k) {
    double t = double(packetIndex * 1024 + k) / double(sampleRate);
    double v = 0.45 * std::sin(2.0 * M_PI * 440.0 * t);
    dst[k] = int16_t(v * 32767.0);
  }
}

void scramble(uint8_t* data, int size, size_t index) {
  if (size <= 8) return;
  for (int k = 0; k < 3; ++k)
    data[size / 2 + k] ^= uint8_t(0xA5 + index + k);
}
} // namespace media::gen
