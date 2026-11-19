#pragma once
// 合成时序与图案：确定性 RNG、逐帧 PTS 计划（漂移/VFR/33 位回绕）、
// YUV 画面与 PCM 音频图案。所有输出可复现。
#include <cstdint>
#include <vector>
#include "media/generator.hpp"

extern "C" {
#include <libavutil/frame.h>
}

namespace media::gen {

struct FramePlan {
  int64_t pts;        // 注入回绕后的原始 PTS（90kHz 滴答）
  int64_t duration;   // 帧时长滴答
  bool keyframe;
  bool corrupt;
  bool wrapped;       // 该帧触发了一次 33 位回绕
};

struct XorShift64 {
  uint64_t state;
  explicit XorShift64(uint64_t seed) : state(seed ? seed : 0x9E3779B97F4A7C15ULL) {}
  uint64_t next() {
    uint64_t x = state;
    x ^= x << 13; x ^= x >> 7; x ^= x << 17;
    state = x;
    return x;
  }
  double unit() { return double(next() >> 11) / double(1ULL << 53); }
};

// 生成一路视频帧计划。ppm 为该相机线性漂移，offsetUs 为常数时钟偏移。
// sidecarPts 与 FramePlan.pts 一致（已回绕）；真值时间（修复后应逼近）
// 为理想 30fps 时间 + 常数偏移。
std::vector<FramePlan> videoPlan(const GenConfig& cfg, uint64_t seed,
                                 double ppm, int64_t offsetUs,
                                 int corruptStride);

// 音频包计划（24kHz、每包 1024 采样），记录原始采样 PTS 与包序号。
struct AudioPlan {
  int64_t pts;
  int64_t samples;
  bool corrupt;
};
std::vector<AudioPlan> audioPlan(const GenConfig& cfg, int corruptStride);

// 填充确定性 YUVJ420P 画面：时间相关渐变 + 每帧序号条纹。
void fillFrame(AVFrame* f, size_t index, int width, int height);

// 填充单声道正弦 PCM（与视频时间同源，便于漂移锚点核对）。
void fillAudio(int16_t* dst, size_t samples, size_t packetIndex,
               int sampleRate);

// 对 MJPEG 码流做确定性字节破坏（在 ffmpeg_util 的 corrupt 钩子里调用）。
void scramble(uint8_t* data, int size, size_t index);
} // namespace media::gen
