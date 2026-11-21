#pragma once

#include <cstdint>
#include <fstream>
#include <string>
#include <vector>

namespace media::fixture {

// 极简 PCM WAV 写入器：16-bit 交错立体声/多声道，供演示与测试夹具使用。
class WavWriter {
 public:
  WavWriter(std::string path, uint16_t channels, uint32_t sampleRate,
            uint16_t bitsPerSample = 16);
  void appendFrames(const std::vector<int32_t>& interleaved);
  void close();
  ~WavWriter() { close(); }

 private:
  std::string path_;
  uint16_t channels_;
  uint32_t sampleRate_;
  uint16_t bits_;
  uint32_t dataBytes_ = 0;
  bool closed_ = false;
  std::ofstream out_;
};

struct FixtureSet {
  std::string stereoPath;       // 正常素材：静音 + 削波 + 重复片段
  std::string truncatedPath;    // 尾部截断
  std::string nonIntegerPath;   // 非整帧尾块
  std::string duplicatePath;    // 内含重复内容
};

// 在 dir 下生成全部夹具；返回各文件绝对路径。
FixtureSet generateAll(const std::string& dir, uint32_t sampleRate = 48000);

}  // namespace media::fixture
