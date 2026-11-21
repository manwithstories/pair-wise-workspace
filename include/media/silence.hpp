#pragma once

#include <cstdint>
#include <vector>

#include "media/audio.hpp"
#include "media/time.hpp"

namespace media {

struct SilenceConfig {
  // 静音阈值（线性幅度，0..2147483647 满幅刻度）；所有声道均低于阈值才算静音帧。
  int32_t threshold = 2147483647 / 1000;  // 约 -60 dBFS
  SampleTick minDurationTicks = 0;        // 短于该长度的静音不报告；0=全部报告
};

struct SilenceRange {
  SampleRange range;
  int64_t durationMs = 0;
};

// 音频质量统计：峰值、RMS、削波样本数（按单个声道样本计）。
struct AudioStats {
  int32_t peak = 0;              // 最大绝对幅度
  double rms = 0.0;              // 全部声道样本的 RMS（满幅刻度）
  uint64_t clippedSamples = 0;   // 命中满幅 INT32_MIN/MAX 的样本数
  uint64_t totalSamples = 0;     // 声道样本总数（帧数 * 声道数）
  uint64_t silentFrames = 0;

  double rmsDb() const;
  double peakDb() const;
};

// 流式静音检测：逐块喂入，边界在 finish() 时确定。
class SilenceDetector {
 public:
  SilenceDetector(SilenceConfig cfg, uint32_t sampleRate);

  void process(const AudioBlock& block);
  // totalFrames 用于截断最后一个开区间；返回合并后的静音区间。
  std::vector<SilenceRange> finish(SampleTick totalFrames);

  const AudioStats& stats() const { return stats_; }
  const SilenceConfig& config() const { return cfg_; }

 private:
  SilenceConfig cfg_;
  uint32_t sampleRate_;
  AudioStats stats_;
  double sumSquares_ = 0.0;
  SampleTick runStart_ = -1;  // 当前连续静音帧区间起点
  std::vector<SilenceRange> ranges_;

  void closeRange(SampleTick end);
};

}  // namespace media
