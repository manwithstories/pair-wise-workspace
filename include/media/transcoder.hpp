#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/audio.hpp"
#include "media/error.hpp"
#include "media/time.hpp"

namespace media {

struct TranscodeConfig {
  std::string outputPath;
  int64_t targetBitrate = 128000;
  uint32_t targetSampleRate = 0;   // 0 表示沿用源采样率
  uint16_t targetChannels = 0;     // 0 表示沿用源声道数
  // 输出容器/编码由扩展名推断（.m4a/.aac/.mp3/.wav）。
};

struct TranscodeResult {
  int64_t targetBitrateBps = 0;
  int64_t actualBitrateBps = 0;   // 按编码载荷字节与整数时长计算
  uint64_t outputBytes = 0;
  SampleTick outputFrames = 0;    // 输出时间线总帧数（整数刻度）
  uint32_t outputSampleRate = 0;
  uint16_t outputChannels = 0;
};

// 单段 PCM 帧的转码输入；samples 为交错 int32（内部满幅刻度）。
struct TranscodeInput {
  const int32_t* samples = nullptr;
  size_t frames = 0;
  int channels = 0;
  uint32_t sampleRate = 0;
};

// 经 libswresample 转换采样率/声道布局，再封装为目标音频格式。
// 全程以整数采样刻度记账，actualBitrate 只用整数帧时长推导，无浮点漂移。
class Transcoder {
 public:
  Transcoder(TranscodeConfig cfg, AudioFormat sourceFormat);
  ~Transcoder();
  Transcoder(const Transcoder&) = delete;
  Transcoder& operator=(const Transcoder&) = delete;

  MediaError open();
  MediaError push(const TranscodeInput& in);  // 可多次调用（逐段）
  MediaError finish(TranscodeResult* result);

  // 截至当前已编码的输出帧总数（输出采样率整数刻度）。
  SampleTick encodedFrames() const;
  uint32_t outputSampleRate() const;

 private:
  struct Impl;
  Impl* impl_;
};

}  // namespace media
