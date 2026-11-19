#include "media.hpp"

#include <cmath>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <memory>
#include <random>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
}

namespace mb {

namespace {
// ---- FFmpeg 资源 RAII，确保异常/早退路径不泄漏 ----
struct FmtCtxDel {
  void operator()(AVFormatContext* c) const {
    if (c) {
      if (c->pb) avio_closep(&c->pb);
      avformat_free_context(c);
    }
  }
};
using FmtPtr = std::unique_ptr<AVFormatContext, FmtCtxDel>;

struct CodecCtxDel {
  void operator()(AVCodecContext* c) const {
    if (c) avcodec_free_context(&c);
  }
};
using CodecPtr = std::unique_ptr<AVCodecContext, CodecCtxDel>;

struct FrameDel {
  void operator()(AVFrame* f) const { if (f) av_frame_free(&f); }
};
using FramePtr = std::unique_ptr<AVFrame, FrameDel>;

struct PktDel {
  void operator()(AVPacket* p) const { if (p) av_packet_free(&p); }
};
using PktPtr = std::unique_ptr<AVPacket, PktDel>;

struct SwrDel {
  void operator()(SwrContext* s) const { if (s) swr_free(&s); }
};
using SwrPtr = std::unique_ptr<SwrContext, SwrDel>;

// 确定性合成图案：每帧 Y 平面统一灰度（随帧号周期变化），色度中灰。
// 因 Y 平面为常量，PSNR 可由均值与参考精确对照，离屏无随机性。
void fill_video_frame(AVFrame* f, int idx, int64_t seed) {
  int h = f->height, w = f->width;
  int luma = static_cast<int>(40 + (idx * 7 + seed) % 180);  // [40,219]
  for (int y = 0; y < h; ++y)
    std::memset(f->data[0] + y * f->linesize[0], luma, w);
  for (int c = 1; c <= 2; ++c)
    for (int y = 0; y < h / 2; ++y)
      std::memset(f->data[c] + y * f->linesize[c], 128, w / 2);
}

const AVCodec* find_encoder(const std::string& name, enum AVCodecID fb) {
  const AVCodec* c = avcodec_find_encoder_by_name(name.c_str());
  if (!c) c = avcodec_find_encoder(fb);
  return c;
}
}  // namespace

bool inject_file_corruption(const std::string& path, int64_t seed,
                            std::string* err) {
  std::fstream f(path, std::ios::in | std::ios::out | std::ios::binary);
  if (!f) { if (err) *err = "cannot open for corruption: " + path;
    return false; }
  f.seekg(0, std::ios::end);
  std::streamoff len = f.tellg();
  if (len <= 256) { if (err) *err = "file too small to corrupt";
    return false; }
  // 在中段确定性位置翻转若干字节（避开容器头部前 256 字节）。
  std::mt19937_64 rng(static_cast<uint64_t>(seed));
  for (int k = 0; k < 8; ++k) {
    std::streamoff off = 256 + rng() % static_cast<uint64_t>(len - 512);
    f.seekp(off);
    char b = static_cast<char>(0xA5 ^ (k * 37));
    f.write(&b, 1);
  }
  f.close();
  return true;
}

bool make_test_media(const MediaSpec& spec, std::string* err) {
  AVFormatContext* rawFmt = nullptr;
  if (avformat_alloc_output_context2(&rawFmt, nullptr, "matroska",
                                     spec.path.c_str()) < 0 || !rawFmt) {
    if (err) *err = "alloc output context failed";
    return false;
  }
  FmtPtr fmt(rawFmt);
  fmt->strict_std_compliance = FF_COMPLIANCE_EXPERIMENTAL;
  const bool global_header =
      (fmt->oformat->flags & AVFMT_GLOBALHEADER) != 0;

  // ---- 视频流（libx264，yuv420p）----
  AVStream* vstr = avformat_new_stream(fmt.get(), nullptr);
  const AVCodec* vcodec = find_encoder("libx264", AV_CODEC_ID_H264);
  if (!vcodec) { if (err) *err = "no h264 encoder"; return false; }
  CodecPtr venc(avcodec_alloc_context3(vcodec));
  venc->width = spec.width;
  venc->height = spec.height;
  venc->pix_fmt = AV_PIX_FMT_YUV420P;
  venc->time_base = AVRational{int(spec.fps.den), int(spec.fps.num)};
  venc->framerate = AVRational{int(spec.fps.num), int(spec.fps.den)};
  venc->bit_rate = spec.video_bitrate;
  venc->rc_buffer_size = spec.video_bitrate;
  av_opt_set(venc->priv_data, "preset", "veryfast", 0);
  av_opt_set(venc->priv_data, "tune", "zerolatency", 0);
  if (global_header) venc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
  if (avcodec_open2(venc.get(), vcodec, nullptr) < 0) {
    if (err) *err = "open video encoder failed";
    return false;
  }
  venc->time_base = AVRational{int(spec.fps.den), int(spec.fps.num)};
  if (avcodec_parameters_from_context(vstr->codecpar, venc.get()) < 0)
    return false;
  vstr->time_base = venc->time_base;

  // ---- 音频流（AAC，可选；缺失音轨由调用方控制）----
  CodecPtr aenc;
  AVStream* astr = nullptr;
  SwrPtr swr;
  FramePtr swr_frame;
  if (spec.include_audio) {
    astr = avformat_new_stream(fmt.get(), nullptr);
    const AVCodec* acodec = find_encoder("aac", AV_CODEC_ID_AAC);
    if (!acodec) { if (err) *err = "no aac encoder"; return false; }
    aenc.reset(avcodec_alloc_context3(acodec));
    aenc->sample_fmt = AV_SAMPLE_FMT_FLTP;
    aenc->sample_rate = spec.audio_sample_rate;
    aenc->bit_rate = spec.audio_bitrate;
    av_channel_layout_default(&aenc->ch_layout, spec.audio_channels);
    if (global_header) aenc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
    if (avcodec_open2(aenc.get(), acodec, nullptr) < 0) {
      if (err) *err = "open audio encoder failed";
      return false;
    }
    if (avcodec_parameters_from_context(astr->codecpar, aenc.get()) < 0)
      return false;
    astr->time_base = AVRational{1, spec.audio_sample_rate};
  }

  if (!(fmt->oformat->flags & AVFMT_NOFILE)) {
    if (avio_open(&fmt->pb, spec.path.c_str(), AVIO_FLAG_WRITE) < 0) {
      if (err) *err = "avio_open failed";
      return false;
    }
  }
  if (avformat_write_header(fmt.get(), nullptr) < 0) {
    if (err) *err = "write header failed";
    return false;
  }

  const int64_t total_frames =
      spec.duration_ms * spec.fps.num / (1000 * spec.fps.den);

  auto drain = [&](AVCodecContext* enc, AVStream* st) {
    AVPacket* pkt = av_packet_alloc();
    while (avcodec_receive_packet(enc, pkt) == 0) {
      av_packet_rescale_ts(pkt, enc->time_base, st->time_base);
      pkt->stream_index = st->index;
      av_interleaved_write_frame(fmt.get(), pkt);
      av_packet_unref(pkt);
    }
    av_packet_free(&pkt);
  };

  // ---- 视频帧编码 ----
  FramePtr vf(av_frame_alloc());
  vf->format = venc->pix_fmt;
  vf->width = spec.width;
  vf->height = spec.height;
  if (av_frame_get_buffer(vf.get(), 0) < 0) return false;
  for (int64_t i = 0; i < total_frames; ++i) {
    if (av_frame_make_writable(vf.get()) < 0) return false;
    fill_video_frame(vf.get(), static_cast<int>(i), spec.corrupt_seed);
    vf->pts = i;
    if (avcodec_send_frame(venc.get(), vf.get()) < 0) return false;
    drain(venc.get(), vstr);
  }
  avcodec_send_frame(venc.get(), nullptr);
  drain(venc.get(), vstr);

  // ---- 音频帧编码（确定性正弦波）----
  if (spec.include_audio) {
    FramePtr af(av_frame_alloc());
    af->format = AV_SAMPLE_FMT_FLTP;
    av_channel_layout_copy(&af->ch_layout, &aenc->ch_layout);
    af->sample_rate = spec.audio_sample_rate;
    af->nb_samples = 1024;
    if (av_frame_get_buffer(af.get(), 0) < 0) return false;
    int64_t total_samples = spec.audio_sample_rate * spec.duration_ms / 1000;
    int64_t made = 0;
    int64_t pts = 0;
    while (made < total_samples) {
      if (av_frame_make_writable(af.get()) < 0) return false;
      int n = static_cast<int>(std::min<int64_t>(1024, total_samples - made));
      for (int s = 0; s < n; ++s) {
        float x = 0.5f * std::sin(2.0f * float(M_PI) * 440.0f *
                                  (made + s) / spec.audio_sample_rate);
        for (int ch = 0; ch < spec.audio_channels; ++ch)
          reinterpret_cast<float*>(af->data[ch])[s] = x;
      }
      af->nb_samples = n;
      af->pts = pts;
      pts += n;
      made += n;
      if (avcodec_send_frame(aenc.get(), af.get()) < 0) return false;
      drain(aenc.get(), astr);
    }
    avcodec_send_frame(aenc.get(), nullptr);
    drain(aenc.get(), astr);
  }

  av_write_trailer(fmt.get());
  if (spec.inject_corruption)
    inject_file_corruption(spec.path, spec.corrupt_seed, nullptr);
  return true;
}

}  // namespace mb
