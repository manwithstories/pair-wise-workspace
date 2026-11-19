#pragma once
// 合成素材生成器：两路 MJPEG 视频 + 一路 PCM 音频，注入时钟偏移/线性漂移/
// 33 位 PTS 回绕/VFR 间隔/周期性坏包，并产出侧车真值与剪辑清单。
#include <cstdint>
#include <string>
#include <vector>
#include "media/manifest.hpp"

namespace media {

struct GenConfig {
  std::string outDir = "demo_out";
  int seconds = 12;
  double driftPpm = 180.0;
  bool wrapPts = true;
  bool vfr = true;
  int corruptEvery = 97;     // >0 时每 N 个包注入一个坏包
  int64_t targetBitrate = 1'800'000;
  int videoWidth = 160;
  int videoHeight = 120;
  int sampleRate = 24000;
  int audioPacketSamples = 1024;
};

struct GenResult {
  std::vector<std::string> files;     // 媒体文件
  std::vector<std::string> sidecars;  // 每轨侧车
  std::string manifestPath;
  int chosenQScale = 0;               // 码率校准选定的 MJPEG 质量
};

// 生成全部素材与清单（纯本地，无网络）。失败返回非空错误消息。
std::string generateSynthetic(const GenConfig& cfg, GenResult& out);
} // namespace media
