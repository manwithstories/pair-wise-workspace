#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/error.hpp"
#include "media/json.hpp"
#include "media/segmenter.hpp"
#include "media/silence.hpp"

namespace media {

struct ManifestGap {
  int64_t byteOffset = -1;   // 损坏在源文件中的字节偏移
  SampleTick sampleIndex = -1;
  std::string code;
  std::string message;
};

struct ManifestSegment {
  int id = 0;
  SampleRange sourceRange;       // 源素材样本偏移（半开）
  SampleRange outputRange;       // 输出时间线样本偏移（半开）
  bool containsGap = false;
  bool duplicate = false;
  int duplicateOf = -1;          // 首个来源片段 id
  uint64_t fingerprint = 0;
};

struct Manifest {
  std::string sourcePath;
  std::string outputPath;
  uint32_t sourceSampleRate = 0;
  uint32_t outputSampleRate = 0;
  uint16_t sourceChannels = 0;
  uint16_t outputChannels = 0;
  uint64_t sourceDigest = 0;
  SampleTick sourceTotalFrames = 0;
  SampleTick outputTotalFrames = 0;
  int64_t targetBitrateBps = 0;
  int64_t actualBitrateBps = 0;
  AudioStats stats;
  std::vector<SilenceRange> silence;
  std::vector<ManifestSegment> segments;
  std::vector<ManifestGap> gaps;
  // 输出时间线与片段边界核对结果：非 0 即验证失败。
  SampleTick alignmentErrorSamples = 0;
};

class ManifestWriter {
 public:
  explicit ManifestWriter(std::string path);
  MediaError write(const Manifest& m) const;
  static json::Value toJson(const Manifest& m);
  // 重新解析清单并核对时间线：返回对齐误差（0 表示完全一致）。
  static SampleTick verifyAlignment(const Manifest& m);

 private:
  std::string path_;
};

}  // namespace media
