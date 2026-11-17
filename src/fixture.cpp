// fixture.cpp — 确定性合成：SplitMix64 噪声 + 双频正弦基底 + 注入异常
#include "fixture.h"

#include <algorithm>
#include <cmath>

namespace popfix {

namespace {

// SplitMix64：确定性、无外部依赖的伪随机源。
class SplitMix64 {
 public:
  explicit SplitMix64(std::uint64_t seed) : state_(seed) {}
  std::uint64_t next() {
    std::uint64_t z = (state_ += 0x9E3779B97F4A7C15ULL);
    z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
    z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
    return z ^ (z >> 31);
  }
  // 归一化到 [-1, 1)。
  double unit() {
    return static_cast<double>(static_cast<int64_t>(next()) >> 11) /
           static_cast<double>(1ULL << 53);
  }

 private:
  std::uint64_t state_;
};

constexpr double kPi = 3.14159265358979323846;

}  // namespace

AudioBuffer generate_fixture(const FixtureSpec& spec) {
  AudioBuffer audio;
  audio.sample_rate = spec.sample_rate;
  audio.channels = spec.channels;
  audio.bits_per_sample = 16;
  audio.data.resize(spec.frames * spec.channels);

  SplitMix64 rng(spec.seed);
  const double dt = 1.0 / static_cast<double>(spec.sample_rate);

  // 健康基底：每声道两个正弦分量 + 幅度 0.02 的噪声，峰值显著低于阈值。
  for (std::size_t f = 0; f < spec.frames; ++f) {
    const double t = static_cast<double>(f) * dt;
    for (std::uint16_t c = 0; c < spec.channels; ++c) {
      const double phase = c == 0 ? 0.0 : 0.7;
      const double f1 = c == 0 ? 220.0 : 247.0;
      const double f2 = c == 0 ? 513.0 : 466.0;
      double v = 0.40 * std::sin(2 * kPi * f1 * t + phase) +
                 0.15 * std::sin(2 * kPi * f2 * t) +
                 0.02 * rng.unit();
      audio.sample(f, c) = std::clamp(v, -0.95, 0.95);
    }
  }

  // 注入三个异常事件（位置远离边界，保证每侧都有充足修复上下文）。
  struct Injection {
    std::size_t start;
    std::size_t length;
    std::uint8_t mask;
    bool positive;
  };
  const Injection injections[3] = {
      {12000, 2, 0x1, true},    // 事件0：左声道短尖峰
      {40000, 48, 0x2, false},  // 事件1：右声道负轨削波（声道分歧）
      {72000, 100, 0x3, true},  // 事件2：双声道连续正轨削波
  };
  for (const Injection& in : injections) {
    for (std::size_t k = 0; k < in.length; ++k) {
      const std::size_t f = in.start + k;
      for (std::uint16_t c = 0; c < spec.channels; ++c) {
        if (in.mask & (1u << c))
          audio.sample(f, c) = in.positive ? 1.0 : -1.0;
      }
    }
  }
  return audio;
}

}  // namespace popfix
