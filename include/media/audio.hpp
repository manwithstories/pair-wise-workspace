#pragma once

#include <cstdint>
#include <vector>

#include "media/time.hpp"

namespace media {

// WAV format tag（wFormatTag）。
constexpr uint16_t kWavTagPcm = 1;
constexpr uint16_t kWavTagIeeeFloat = 3;
constexpr uint16_t kWavTagExtensible = 0xFFFE;

// 内部统一采样编码：所有位深转换为带符号 32-bit 整数（满幅对齐）。
enum class SampleEncoding { U8, S16, S24, S32, F32, F64 };

struct AudioFormat {
  uint16_t formatTag = 0;       // 原始 wFormatTag（Extensible 已解析为子格式）
  uint16_t channels = 0;
  uint32_t sampleRate = 0;
  uint16_t bitsPerSample = 0;   // 容器位深（8/16/24/32/64）
  uint32_t channelMask = 0;     // Extensible 声道掩码；0 表示默认布局
  SampleEncoding encoding = SampleEncoding::S16;

  uint16_t frameSize() const {
    return static_cast<uint16_t>(channels * (bitsPerSample / 8));
  }
  bool isPcmInteger() const {
    return formatTag == kWavTagPcm;
  }
};

// 交错采样块：samples 长度 = frames * channels，首帧绝对序号 firstSample。
struct AudioBlock {
  const int32_t* samples = nullptr;
  size_t frames = 0;
  int channels = 0;
  SampleTick firstSample = 0;
};

// FNV-1a 64 位哈希，用于输入摘要与内容指纹。
class FnvHash {
 public:
  static constexpr uint64_t kOffsetBasis = 1469598103934665603ULL;
  static constexpr uint64_t kPrime = 1099511628211ULL;

  FnvHash() = default;
  explicit FnvHash(uint64_t seed) : value_(seed) {}

  void update(const void* data, size_t size) {
    const auto* p = static_cast<const unsigned char*>(data);
    for (size_t i = 0; i < size; ++i) {
      value_ ^= p[i];
      value_ *= kPrime;
    }
  }
  uint64_t value() const { return value_; }

 private:
  uint64_t value_ = kOffsetBasis;
};

}  // namespace media
