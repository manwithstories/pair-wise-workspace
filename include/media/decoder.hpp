#pragma once
// MJPEG 解码与逐帧质量度量：解码合成 MKV，按侧车序号对齐参考帧计算 Y 平面
// MSE/PSNR；解码失败计入损坏帧并赋保守 MSE，使 PSNR 落在可解释区间。
#include <cstdint>
#include <functional>
#include <string>
#include <vector>
#include "media/quality_report.hpp"

namespace media {

struct DecodeOutcome {
  size_t framesDecoded = 0;
  size_t framesCorrupt = 0; // 解码失败或侧车标注损坏的帧数
  double meanMse = 0.0;     // 全部帧平均 MSE（损坏帧按保守值计入）
  PsnrAccumulator psnr;
};

// 逐帧解码回调：拿到 Y 平面指针、行跨度与帧序号。
using YuvFrameFn = std::function<void(const uint8_t* y, int linesize,
                                      int width, int height, size_t index)>;

// 解码 MJPEG/MKV，统计帧数；对每帧调用 fn（可空）。
struct DecodeSummary {
  size_t frames = 0;
  size_t decodeFailures = 0;
  int width = 0;
  int height = 0;
};
DecodeSummary decodeMjpeg(const std::string& path, YuvFrameFn fn);

// 生成与 gen_patterns 完全一致的参考 Y 平面（单平面、紧凑行）。
std::vector<uint8_t> referenceLuma(size_t index, int width, int height);

// 两平面 MSE（y 为解码紧凑拷贝，ref 为参考）。
double lumaMse(const uint8_t* y, int linesize,
               const std::vector<uint8_t>& ref, int width, int height);

// 汇总解码结果并按侧车损坏标注合并，得到 PSNR 与损坏帧数。
DecodeOutcome measureVideo(const std::string& mkvPath,
                           const std::vector<bool>& sidecarCorrupt);
} // namespace media
