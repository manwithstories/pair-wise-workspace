#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/checkpoint.hpp"
#include "media/error.hpp"
#include "media/fingerprint.hpp"
#include "media/manifest.hpp"
#include "media/segmenter.hpp"
#include "media/silence.hpp"
#include "media/transcoder.hpp"
#include "media/wav_reader.hpp"

namespace media {

// 进程退出码：0 验证通过；2 素材损坏但按策略恢复完成；其他非零=失败。
enum class ExitStatus : int {
  Ok = 0,
  CorruptionRecovered = 2,
  Failure = 1,
};

struct PipelineConfig {
  std::string inputPath;
  std::string outputPath;
  std::string manifestPath;
  std::string checkpointPath;
  std::string workDir;             // 路径限定根目录
  int64_t targetBitrate = 192000;
  uint32_t targetSampleRate = 0;   // 0=沿用源
  uint16_t targetChannels = 0;     // 0=沿用源
  SampleTick maxSegmentSamples = 0;
  int64_t targetDurationMs = 0;     // 0=不按时长切；内部整数换算为刻度
  SampleTick targetDurationTicks = 0;
  bool splitOnSilence = false;
  SilenceConfig silence;
  FingerprintConfig fingerprint;
  CorruptionPolicy corruptionPolicy = CorruptionPolicy::Skip;
  bool resume = true;
};

struct PipelineResult {
  ExitStatus status = ExitStatus::Failure;
  int segmentsCount = 0;
  int recoverableSegments = 0;
  int duplicatesCount = 0;
  int64_t silenceMs = 0;
  uint64_t clippedSamples = 0;
  int64_t targetBitrateBps = 0;
  int64_t actualBitrateBps = 0;
  SampleTick alignmentErrorSamples = -1;
  int64_t badOffsetBytes = -1;     // 首个损坏偏移；无损坏为 -1
  int segmentsBeforeRecovery = 0;  // 恢复前已完成片段数
  bool resumed = false;
  std::vector<MediaError> recoverableErrors;
  MediaError fatal;
};

class Pipeline {
 public:
  explicit Pipeline(PipelineConfig cfg);
  PipelineResult run();

 private:
  MediaError validatePaths();
  CheckpointParams buildParams(const WavSummary& sum) const;
  std::vector<SampleRange> collectGaps(const std::vector<MediaError>& errs) const;

  PipelineConfig cfg_;
};

}  // namespace media
