#pragma once

#include <cstdint>
#include <vector>

#include "media/audio.hpp"
#include "media/segmenter.hpp"
#include "media/time.hpp"

namespace media {

struct FingerprintConfig {
  SampleTick windowTicks = 1024;  // 规范化窗口长度（采样帧）
  double matchThreshold = 0.8;    // 窗口哈希重合比例达到该值即判重
};

struct SegmentFingerprint {
  int segmentId = 0;
  SampleRange range;
  std::vector<uint64_t> windowHashes;
  uint64_t signature = 0;  // 窗口哈希序列的组合指纹
};

struct DuplicateMatch {
  int segmentId = 0;        // 重复片段
  int firstSourceId = 0;    // 首个来源片段
  SampleRange sourceRange;  // 首个来源的时间范围（整数采样刻度）
  double score = 0.0;       // 重合窗口比例
};

// 对规范化采样窗口生成稳定 FNV 指纹，并按窗口序列重合度判重。
// 重复片段返回匹配记录，调用方据此不重复写入输出。
class Fingerprinter {
 public:
  explicit Fingerprinter(FingerprintConfig cfg);

  SegmentFingerprint build(const Segment& seg,
                           const std::vector<int32_t>& interleaved,
                           int channels) const;

  // 与已登记指纹比较；命中返回 true 并填充 match，否则登记新指纹。
  bool findOrRegister(const SegmentFingerprint& fp, DuplicateMatch* match);

  const std::vector<SegmentFingerprint>& known() const { return known_; }

 private:
  FingerprintConfig cfg_;
  std::vector<SegmentFingerprint> known_;
};

}  // namespace media
