#include "media/fixture_generator.hpp"

#include "media/timeline.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/channel_layout.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
}

#include <cstring>
#include <string>

namespace mediaqc {
namespace {

std::string ffmpeg_error(int err) {
  char buffer[AV_ERROR_MAX_STRING_SIZE] = {0};
  av_strerror(err, buffer, sizeof buffer);
  return buffer;
}

// A small deterministic hash so generated content is reproducible without
// depending on the C library's random implementation.
std::uint32_t mix(std::uint64_t seed, std::uint64_t index) {
  std::uint64_t x = seed * 0x9e3779b97f4a7c15ULL + index * 0xbf58476d1ce4e5b9ULL;
  x ^= x >> 30;
  x *= 0xbf58476d1ce4e5b9ULL;
  x ^= x >> 27;
  x *= 0x94d049bb133111ebULL;
  x ^= x >> 31;
  return static_cast<std::uint32_t>(x);
}

bool open_output(const std::string& path, AVFormatContext*& format,
                 std::string& error) {
  int ret = avformat_alloc_output_context2(&format, nullptr, "matroska",
                                           path.c_str());
  if (ret < 0 || format == nullptr) {
    error = "cannot allocate output context: " + ffmpeg_error(ret);
    return false;
  }
  ret = avio_open(&format->pb, path.c_str(), AVIO_FLAG_WRITE);
  if (ret < 0) {
    error = "cannot open " + path + ": " + ffmpeg_error(ret);
    avformat_free_context(format);
    format = nullptr;
    return false;
  }
  return true;
}

void drain_encoder(AVCodecContext* context, AVPacket* packet, AVStream* stream,
                   AVFormatContext* format,
                   std::vector<std::vector<std::uint8_t>>* out,
                   bool& failed, std::string& error) {
  while (avcodec_receive_packet(context, packet) == 0) {
    packet->stream_index = stream->index;
    av_packet_rescale_ts(packet, context->time_base, stream->time_base);
    if (out) {
      out->emplace_back(packet->data, packet->data + packet->size);
    }
    int ret = av_interleaved_write_frame(format, packet);
    av_packet_unref(packet);
    if (ret < 0 && !failed) {
      failed = true;
      error = "write failed: " + ffmpeg_error(ret);
    }
  }
}

}  // namespace

std::vector<std::uint8_t> make_video_frame(const VideoSpec& spec,
                                           std::uint64_t index) {
  const std::size_t y_size =
      static_cast<std::size_t>(spec.width) * static_cast<std::size_t>(spec.height);
  const std::size_t chroma_width = static_cast<std::size_t>((spec.width + 1) / 2);
  const std::size_t chroma_height = static_cast<std::size_t>((spec.height + 1) / 2);
  std::vector<std::uint8_t> frame(y_size + chroma_width * chroma_height * 2);
  std::uint32_t base = mix(spec.seed, index);
  for (int y = 0; y < spec.height; ++y) {
    for (int x = 0; x < spec.width; ++x) {
      frame[static_cast<std::size_t>(y) * spec.width + x] =
          static_cast<std::uint8_t>((base >> 3) + x * 3 + y * 5 + index);
    }
  }
  std::uint8_t* u = frame.data() + y_size;
  std::uint8_t* v = u + chroma_width * chroma_height;
  for (std::size_t i = 0; i < chroma_width * chroma_height; ++i) {
    u[i] = static_cast<std::uint8_t>(96 + (base & 0x3f) + index);
    v[i] = static_cast<std::uint8_t>(160 - (base & 0x1f));
  }
  return frame;
}

std::vector<std::uint8_t> make_audio_frame(const AudioSpec& spec,
                                           std::uint64_t index) {
  const std::size_t samples = static_cast<std::size_t>(spec.frames_per_packet);
  const std::size_t channels = static_cast<std::size_t>(spec.channels);
  std::vector<std::uint8_t> bytes(samples * channels * 2);
  std::uint32_t base = mix(spec.seed ^ 0xa5a5a5a5U, index);
  for (std::size_t s = 0; s < samples; ++s) {
    std::int16_t sample = static_cast<std::int16_t>(
        ((s * 37 + base) % 2001) - 1000);
    for (std::size_t c = 0; c < channels; ++c) {
      std::size_t at = (s * channels + c) * 2;
      bytes[at] = static_cast<std::uint8_t>(sample & 0xff);
      bytes[at + 1] = static_cast<std::uint8_t>((sample >> 8) & 0xff);
    }
  }
  return bytes;
}

bool write_video_container(const std::string& path, const VideoSpec& spec,
                           std::vector<std::vector<std::uint8_t>>* out_payloads,
                           std::string& error) {
  const AVCodec* codec = avcodec_find_encoder(AV_CODEC_ID_FFV1);
  if (!codec) {
    error = "FFV1 encoder unavailable";
    return false;
  }
  AVCodecContext* context = avcodec_alloc_context3(codec);
  if (!context) {
    error = "cannot allocate FFV1 context";
    return false;
  }
  context->width = spec.width;
  context->height = spec.height;
  context->pix_fmt = AV_PIX_FMT_YUV420P;
  context->time_base = AVRational{1, static_cast<int>(kNsPerSecond)};
  context->framerate = AVRational{spec.rate.num, spec.rate.den};
  context->gop_size = 10;
  // Single-threaded encoding keeps output byte-identical across runs.
  context->thread_count = 1;
  if (avcodec_open2(context, codec, nullptr) < 0) {
    error = "cannot open FFV1 encoder";
    avcodec_free_context(&context);
    return false;
  }

  AVFormatContext* format = nullptr;
  bool ok = open_output(path, format, error);
  AVStream* stream = nullptr;
  if (ok) {
    stream = avformat_new_stream(format, nullptr);
    if (!stream || avcodec_parameters_from_context(stream->codecpar, context) < 0) {
      error = "cannot create video stream";
      ok = false;
    } else {
      stream->time_base = context->time_base;
    }
  }
  if (ok) {
    int ret = avformat_write_header(format, nullptr);
    if (ret < 0) {
      error = "cannot write header: " + ffmpeg_error(ret);
      ok = false;
    }
  }

  const Ns interval = TrackTimeline::compute_interval_ns(spec.rate);
  AVFrame* frame = ok ? av_frame_alloc() : nullptr;
  AVPacket* packet = ok ? av_packet_alloc() : nullptr;
  bool failed = false;
  std::int64_t pts = 0;
  for (std::uint64_t i = 0; ok && !failed && i < spec.frame_count; ++i) {
    frame->format = context->pix_fmt;
    frame->width = spec.width;
    frame->height = spec.height;
    if (av_frame_get_buffer(frame, 32) < 0) {
      error = "cannot allocate frame buffer";
      failed = true;
      break;
    }
    std::vector<std::uint8_t> payload = make_video_frame(spec, i);
    const int chroma_w = (spec.width + 1) / 2;
    const int chroma_h = (spec.height + 1) / 2;
    for (int y = 0; y < spec.height; ++y) {
      std::memcpy(frame->data[0] + static_cast<std::size_t>(y) * frame->linesize[0],
                  payload.data() + static_cast<std::size_t>(y) * spec.width,
                  static_cast<std::size_t>(spec.width));
    }
    const std::uint8_t* u = payload.data() +
                            static_cast<std::size_t>(spec.width) * spec.height;
    const std::uint8_t* v = u + static_cast<std::size_t>(chroma_w) * chroma_h;
    for (int y = 0; y < chroma_h; ++y) {
      std::memcpy(frame->data[1] + static_cast<std::size_t>(y) * frame->linesize[1],
                  u + static_cast<std::size_t>(y) * chroma_w,
                  static_cast<std::size_t>(chroma_w));
      std::memcpy(frame->data[2] + static_cast<std::size_t>(y) * frame->linesize[2],
                  v + static_cast<std::size_t>(y) * chroma_w,
                  static_cast<std::size_t>(chroma_w));
    }
    frame->pts = pts;
    frame->duration = interval;
    pts += interval;
    if (avcodec_send_frame(context, frame) < 0) {
      error = "encode send failed";
      failed = true;
    }
    av_frame_free(&frame);
    frame = av_frame_alloc();
    if (!failed) {
      drain_encoder(context, packet, stream, format, out_payloads, failed,
                    error);
    }
  }

  if (ok && !failed) {
    avcodec_send_frame(context, nullptr);
    drain_encoder(context, packet, stream, format, out_payloads, failed, error);
  }
  if (frame) av_frame_free(&frame);
  if (packet) av_packet_free(&packet);
  if (format) {
    if (ok && !failed) {
      int ret = av_write_trailer(format);
      if (ret < 0) {
        error = "cannot write trailer: " + ffmpeg_error(ret);
        failed = true;
      }
    }
    if (format->pb) avio_closep(&format->pb);
    avformat_free_context(format);
  }
  avcodec_free_context(&context);
  return ok && !failed;
}

bool write_audio_container(const std::string& path, const AudioSpec& spec,
                           std::vector<std::vector<std::uint8_t>>* out_payloads,
                           std::string& error) {
  const AVCodec* codec = avcodec_find_encoder(AV_CODEC_ID_PCM_S16LE);
  if (!codec) {
    error = "PCM s16le encoder unavailable";
    return false;
  }
  AVCodecContext* context = avcodec_alloc_context3(codec);
  if (!context) {
    error = "cannot allocate PCM context";
    return false;
  }
  context->sample_fmt = AV_SAMPLE_FMT_S16;
  context->sample_rate = spec.sample_rate;
  av_channel_layout_default(&context->ch_layout, spec.channels);
  context->time_base = AVRational{1, spec.sample_rate};
  context->thread_count = 1;
  if (avcodec_open2(context, codec, nullptr) < 0) {
    error = "cannot open PCM encoder";
    avcodec_free_context(&context);
    return false;
  }

  AVFormatContext* format = nullptr;
  bool ok = open_output(path, format, error);
  AVStream* stream = nullptr;
  if (ok) {
    stream = avformat_new_stream(format, nullptr);
    if (!stream || avcodec_parameters_from_context(stream->codecpar, context) < 0) {
      error = "cannot create audio stream";
      ok = false;
    } else {
      stream->time_base = context->time_base;
    }
  }
  if (ok) {
    int ret = avformat_write_header(format, nullptr);
    if (ret < 0) {
      error = "cannot write header: " + ffmpeg_error(ret);
      ok = false;
    }
  }

  AVPacket* packet = ok ? av_packet_alloc() : nullptr;
  bool failed = false;
  std::int64_t pts = 0;
  for (std::uint64_t i = 0; ok && !failed && i < spec.frame_count; ++i) {
    std::vector<std::uint8_t> bytes = make_audio_frame(spec, i);
    AVFrame* frame = av_frame_alloc();
    frame->format = AV_SAMPLE_FMT_S16;
    frame->sample_rate = spec.sample_rate;
    av_channel_layout_default(&frame->ch_layout, spec.channels);
    frame->nb_samples = static_cast<int>(spec.frames_per_packet);
    if (av_frame_get_buffer(frame, 0) < 0) {
      error = "cannot allocate audio buffer";
      av_frame_free(&frame);
      failed = true;
      break;
    }
    std::memcpy(frame->data[0], bytes.data(), bytes.size());
    frame->pts = pts;
    pts += frame->nb_samples;
    if (avcodec_send_frame(context, frame) < 0) {
      error = "audio encode send failed";
      failed = true;
    }
    av_frame_free(&frame);
    if (!failed) {
      drain_encoder(context, packet, stream, format, out_payloads, failed,
                    error);
    }
  }
  if (ok && !failed) {
    avcodec_send_frame(context, nullptr);
    drain_encoder(context, packet, stream, format, out_payloads, failed, error);
  }
  if (packet) av_packet_free(&packet);
  if (format) {
    if (ok && !failed) {
      int ret = av_write_trailer(format);
      if (ret < 0) {
        error = "cannot write trailer: " + ffmpeg_error(ret);
        failed = true;
      }
    }
    if (format->pb) avio_closep(&format->pb);
    avformat_free_context(format);
  }
  avcodec_free_context(&context);
  return ok && !failed;
}

}  // namespace mediaqc
