#pragma once
// FFmpeg RAII 封装与最小编码工具：MKV/MJPEG 视频与 WAV/PCM 音频。
// 仅服务本地合成素材生成，不接触网络。
#include <cstdint>
#include <functional>
#include <string>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/frame.h>
#include <libavutil/imgutils.h>
}

namespace media::ff {

std::string avError(int rc);

struct CodecCtx {
  AVCodecContext* p = nullptr;
  CodecCtx() = default;
  ~CodecCtx() { if (p) avcodec_free_context(&p); }
  CodecCtx(const CodecCtx&) = delete;
  CodecCtx& operator=(const CodecCtx&) = delete;
};

struct Frame {
  AVFrame* p = nullptr;
  Frame() { p = av_frame_alloc(); }
  ~Frame() { if (p) av_frame_free(&p); }
  Frame(const Frame&) = delete;
  Frame& operator=(const Frame&) = delete;
};

struct Packet {
  AVPacket* p = nullptr;
  Packet() { p = av_packet_alloc(); }
  ~Packet() { if (p) av_packet_free(&p); }
  Packet(const Packet&) = delete;
  Packet& operator=(const Packet&) = delete;
};

struct VideoEncodeSpec {
  int width = 160;
  int height = 120;
  AVRational timeBase{1, 90000};
  int qscale = 5; // MJPEG 质量（1 最好，31 最差）
};

// 逐帧回调式 MJPEG 编码到 MKV。
//   pts(i)     返回该帧在 timeBase 下的 PTS
//   fill(frame,i) 填充 YUV420P 内容
//   corrupt(i)  为 true 时破坏该帧码流字节
// 成功返回空串，失败返回错误消息。
std::string encodeMJpeg(
    const std::string& path, const VideoEncodeSpec& spec, size_t frameCount,
    const std::function<void(AVFrame*, size_t)>& fill,
    const std::function<int64_t(size_t)>& pts,
    const std::function<bool(size_t)>& corrupt);

// 交错 s16 PCM 编码为 WAV。fill 写入 n 个采样（含全部声道）。
std::string encodePcmWav(
    const std::string& path, int sampleRate, int channels,
    size_t totalSamples,
    const std::function<void(int16_t*, size_t)>& fill);

} // namespace media::ff
