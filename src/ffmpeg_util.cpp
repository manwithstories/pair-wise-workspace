#include "media/ffmpeg_util.hpp"
#include <algorithm>
#include <cstring>

extern "C" {
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
}

namespace media::ff {

std::string avError(int rc) {
  char buf[AV_ERROR_MAX_STRING_SIZE] = {0};
  av_strerror(rc, buf, sizeof(buf));
  return buf;
}

std::string encodeMJpeg(
    const std::string& path, const VideoEncodeSpec& spec, size_t frameCount,
    const std::function<void(AVFrame*, size_t)>& fill,
    const std::function<int64_t(size_t)>& ptsFn,
    const std::function<bool(size_t)>& corrupt) {
  AVFormatContext* fmt = nullptr;
  int rc = avformat_alloc_output_context2(&fmt, nullptr, nullptr, path.c_str());
  if (rc < 0) return "alloc output: " + avError(rc);

  const AVCodec* codec = avcodec_find_encoder(AV_CODEC_ID_MJPEG);
  if (!codec) return "MJPEG encoder unavailable";
  CodecCtx enc;
  enc.p = avcodec_alloc_context3(codec);
  enc.p->width = spec.width;
  enc.p->height = spec.height;
  enc.p->pix_fmt = AV_PIX_FMT_YUVJ420P;
  enc.p->time_base = spec.timeBase;
  enc.p->framerate = AVRational{30, 1};
  enc.p->flags |= AV_CODEC_FLAG_QSCALE;
  enc.p->global_quality = spec.qscale * FF_QP2LAMBDA;

  AVStream* st = avformat_new_stream(fmt, nullptr);
  if (!st) return "new stream failed";
  if ((rc = avcodec_open2(enc.p, codec, nullptr)) < 0)
    return "open encoder: " + avError(rc);
  avcodec_parameters_from_context(st->codecpar, enc.p);
  st->time_base = spec.timeBase;

  if (!(fmt->oformat->flags & AVFMT_NOFILE))
    if ((rc = avio_open(&fmt->pb, path.c_str(), AVIO_FLAG_WRITE)) < 0)
      return "avio_open: " + avError(rc);
  if ((rc = avformat_write_header(fmt, nullptr)) < 0)
    return "write_header: " + avError(rc);

  Frame frame;
  Packet pkt;
  size_t encodedIndex = 0;
  auto drain = [&]() -> std::string {
    while (true) {
      rc = avcodec_receive_packet(enc.p, pkt.p);
      if (rc == AVERROR(EAGAIN) || rc == AVERROR_EOF) break;
      if (rc < 0) return "receive_packet: " + avError(rc);
      if (encodedIndex < frameCount && corrupt(encodedIndex) &&
          pkt.p->size > 16) {
        // 只在码流安全区内做字节破坏，绝不越界写。
        int base = pkt.p->size / 2;
        for (int k = 0; k < 3 && base + k < pkt.p->size; ++k)
          pkt.p->data[base + k] ^= uint8_t(0xA5 + k);
      }
      ++encodedIndex;
      av_packet_rescale_ts(pkt.p, enc.p->time_base, st->time_base);
      if ((rc = av_interleaved_write_frame(fmt, pkt.p)) < 0)
        return "write_frame: " + avError(rc);
      av_packet_unref(pkt.p);
    }
    return {};
  };

  for (size_t i = 0; i < frameCount; ++i) {
    // av_frame_unref 会清空这些字段，每帧必须重新设置。
    frame.p->format = enc.p->pix_fmt;
    frame.p->width = spec.width;
    frame.p->height = spec.height;
    if ((rc = av_frame_get_buffer(frame.p, 0)) < 0)
      return "frame buffer: " + avError(rc);
    if ((rc = av_frame_make_writable(frame.p)) < 0)
      return "frame writable: " + avError(rc);
    fill(frame.p, i);
    frame.p->pts = ptsFn(i);
    if ((rc = avcodec_send_frame(enc.p, frame.p)) < 0)
      return "send_frame: " + avError(rc);
    if (auto e = drain(); !e.empty()) return e;
    av_frame_unref(frame.p);
  }
  avcodec_send_frame(enc.p, nullptr);
  if (auto e = drain(); !e.empty()) return e;
  av_write_trailer(fmt);
  if (!(fmt->oformat->flags & AVFMT_NOFILE)) avio_closep(&fmt->pb);
  avformat_free_context(fmt);
  return {};
}

std::string encodePcmWav(
    const std::string& path, int sampleRate, int channels,
    size_t totalSamples,
    const std::function<void(int16_t*, size_t)>& fill) {
  AVFormatContext* fmt = nullptr;
  int rc = avformat_alloc_output_context2(&fmt, nullptr, nullptr, path.c_str());
  if (rc < 0) return "alloc wav: " + avError(rc);
  const AVCodec* codec = avcodec_find_encoder(AV_CODEC_ID_PCM_S16LE);
  if (!codec) return "PCM s16le encoder unavailable";
  CodecCtx enc;
  enc.p = avcodec_alloc_context3(codec);
  enc.p->sample_rate = sampleRate;
  enc.p->sample_fmt = AV_SAMPLE_FMT_S16;
  av_channel_layout_default(&enc.p->ch_layout, channels);
  enc.p->time_base = AVRational{1, sampleRate};

  AVStream* st = avformat_new_stream(fmt, nullptr);
  if ((rc = avcodec_open2(enc.p, codec, nullptr)) < 0)
    return "open pcm: " + avError(rc);
  avcodec_parameters_from_context(st->codecpar, enc.p);
  st->time_base = enc.p->time_base;
  if ((rc = avio_open(&fmt->pb, path.c_str(), AVIO_FLAG_WRITE)) < 0)
    return "avio wav: " + avError(rc);
  if ((rc = avformat_write_header(fmt, nullptr)) < 0)
    return "wav header: " + avError(rc);

  Frame frame;
  Packet pkt;
  const size_t chunk = 1024;
  int64_t pts = 0;
  for (size_t off = 0; off < totalSamples; off += chunk) {
    size_t n = std::min(chunk, totalSamples - off);
    frame.p->nb_samples = int(n);
    frame.p->format = enc.p->sample_fmt;
    frame.p->sample_rate = sampleRate;
    av_channel_layout_copy(&frame.p->ch_layout, &enc.p->ch_layout);
    if ((rc = av_frame_get_buffer(frame.p, 0)) < 0)
      return "audio buffer: " + avError(rc);
    av_frame_make_writable(frame.p);
    fill(reinterpret_cast<int16_t*>(frame.p->data[0]), n);
    frame.p->pts = pts;
    pts += int64_t(n);
    avcodec_send_frame(enc.p, frame.p);
    while (avcodec_receive_packet(enc.p, pkt.p) == 0) {
      av_packet_rescale_ts(pkt.p, enc.p->time_base, st->time_base);
      av_interleaved_write_frame(fmt, pkt.p);
      av_packet_unref(pkt.p);
    }
    av_frame_unref(frame.p);
  }
  av_write_trailer(fmt);
  avio_closep(&fmt->pb);
  avformat_free_context(fmt);
  return {};
}
} // namespace media::ff
