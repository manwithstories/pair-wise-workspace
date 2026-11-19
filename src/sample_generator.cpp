#include <cmath>
#include <cstring>
#include <string>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
}

#include "media_archive/sample_generator.h"

namespace media_archive {

const char* codec_extension(CodecType codec) {
  switch (codec) {
    case CodecType::H264: return "h264";
    case CodecType::H265: return "h265";
    case CodecType::AV1:  return "av1";
    case CodecType::Unknown: return "bin";
  }
  return "bin";
}

namespace {

AVCodecID to_av_codec(CodecType codec) {
  switch (codec) {
    case CodecType::H264: return AV_CODEC_ID_H264;
    case CodecType::H265: return AV_CODEC_ID_HEVC;
    case CodecType::AV1:  return AV_CODEC_ID_AV1;
    case CodecType::Unknown: return AV_CODEC_ID_NONE;
  }
  return AV_CODEC_ID_NONE;
}

const char* encoder_name(CodecType codec) {
  switch (codec) {
    case CodecType::H264: return "libx264";
    case CodecType::H265: return "libx265";
    case CodecType::AV1:  return "libsvtav1";
    case CodecType::Unknown: return nullptr;
  }
  return nullptr;
}

// Deterministic synthetic content: a moving gradient band plus a frame
// counter so consecutive frames differ and the encoder emits real inter
// frames, while identical seeds reproduce identical input frames.
void fill_frame(AVFrame* frame, uint32_t index, uint32_t width,
                uint32_t height) {
  for (uint32_t y = 0; y < height; ++y) {
    uint8_t* row = frame->data[0] + y * frame->linesize[0];
    for (uint32_t x = 0; x < width; ++x) {
      const uint32_t band = (x + index * 2) % width;
      row[x] = static_cast<uint8_t>((x * 2 + y + band) & 0xFF);
    }
  }
  for (int plane = 1; plane <= 2; ++plane) {  // Cb/Cr, YUV420p
    const uint32_t pw = width / 2;
    const uint32_t ph = height / 2;
    for (uint32_t y = 0; y < ph; ++y) {
      uint8_t* row = frame->data[plane] + y * frame->linesize[plane];
      for (uint32_t x = 0; x < pw; ++x)
        row[x] = static_cast<uint8_t>((x * 7 + y * 11 + index) & 0xFF);
    }
  }
}

Diagnostic configure_encoder(AVCodecContext* ctx, CodecType codec,
                             const SampleConfig& cfg) {
  ctx->width = static_cast<int>(cfg.width);
  ctx->height = static_cast<int>(cfg.height);
  ctx->time_base = AVRational{1, static_cast<int>(kTimebaseHz)};
  ctx->framerate = AVRational{static_cast<int>(cfg.fps), 1};
  ctx->pix_fmt = AV_PIX_FMT_YUV420P;
  ctx->gop_size = static_cast<int>(cfg.gop);
  ctx->max_b_frames = 0;  // display order equals packet order
  ctx->bit_rate = 400'000;

  AVDictionary* opts = nullptr;
  if (codec == CodecType::AV1) {
    // libsvtav1: numeric preset, integer tune, and an explicit rate-control
    // mode because its CRF mode rejects a target bitrate.
    av_dict_set(&opts, "preset", "8", 0);
    av_dict_set(&opts, "tune", "0", 0);
    av_dict_set(&opts, "rc", "1", 0);  // VBR
  } else {
    av_dict_set(&opts, "preset", "veryfast", 0);
    av_dict_set(&opts, "tune", "zerolatency", 0);
    av_dict_set(&opts, "crf", "23", 0);
  }
  av_dict_set(&opts, "threads", "1", 0);  // deterministic output
  const int rc = avcodec_open2(ctx, nullptr, &opts);
  av_dict_free(&opts);
  if (rc < 0)
    return diag(Status::UsageError,
                std::string("cannot open encoder ") + encoder_name(codec));
  return {};
}

}  // namespace

Diagnostic encode_video(const SampleConfig& cfg,
                        std::vector<EncodedFrame>* out) {
  const AVCodec* codec = avcodec_find_encoder(to_av_codec(cfg.codec));
  if (!codec)
    return diag(Status::UsageError,
                std::string("encoder unavailable: ") +
                    std::string(encoder_name(cfg.codec)));
  AVCodecContext* ctx = avcodec_alloc_context3(codec);
  if (!ctx) return diag(Status::IoError, "cannot allocate encoder context");
  Diagnostic cd = configure_encoder(ctx, cfg.codec, cfg);
  if (!cd.ok()) {
    avcodec_free_context(&ctx);
    return cd;
  }

  AVFrame* frame = av_frame_alloc();
  frame->format = ctx->pix_fmt;
  frame->width = ctx->width;
  frame->height = ctx->height;
  if (av_frame_get_buffer(frame, 0) < 0) {
    av_frame_free(&frame);
    avcodec_free_context(&ctx);
    return diag(Status::IoError, "cannot allocate frame buffer");
  }
  AVPacket* pkt = av_packet_alloc();
  const uint64_t pts_step = kTimebaseHz / cfg.fps;

  auto drain = [&]() -> Diagnostic {
    while (true) {
      const int r = avcodec_receive_packet(ctx, pkt);
      if (r == AVERROR(EAGAIN) || r == AVERROR_EOF) break;
      if (r < 0) return diag(Status::IoError, "video encoding failed");
      EncodedFrame ef;
      ef.bytes.assign(pkt->data, pkt->data + pkt->size);
      ef.keyframe = (pkt->flags & AV_PKT_FLAG_KEY) != 0;
      out->push_back(std::move(ef));
      av_packet_unref(pkt);
    }
    return {};
  };

  Diagnostic failure;
  for (uint32_t i = 0; i < cfg.frame_count && failure.ok(); ++i) {
    if (av_frame_make_writable(frame) < 0) {
      failure = diag(Status::IoError, "frame not writable");
      break;
    }
    fill_frame(frame, i, cfg.width, cfg.height);
    frame->pts = static_cast<int64_t>(i * pts_step);
    if (avcodec_send_frame(ctx, frame) < 0) {
      failure = diag(Status::IoError, "send_frame failed");
      break;
    }
    failure = drain();
  }
  if (failure.ok()) {
    avcodec_send_frame(ctx, nullptr);  // flush
    failure = drain();
  }

  av_packet_free(&pkt);
  av_frame_free(&frame);
  avcodec_free_context(&ctx);
  if (failure.ok() && out->size() != cfg.frame_count)
    return diag(Status::IoError, "encoder produced an unexpected frame count");
  return failure;
}

std::vector<uint8_t> frame_wire_bytes(CodecType codec,
                                      const EncodedFrame& frame) {
  if (codec != CodecType::AV1) return frame.bytes;  // already Annex B
  // AV1 length-delimited temporal units: 4 byte big-endian frame length
  // followed by the OBUs, which is what the av1 parser splits on.
  std::vector<uint8_t> wire;
  wire.reserve(frame.bytes.size() + 4);
  const uint32_t n = static_cast<uint32_t>(frame.bytes.size());
  wire.push_back(static_cast<uint8_t>((n >> 24) & 0xFF));
  wire.push_back(static_cast<uint8_t>((n >> 16) & 0xFF));
  wire.push_back(static_cast<uint8_t>((n >> 8) & 0xFF));
  wire.push_back(static_cast<uint8_t>(n & 0xFF));
  wire.insert(wire.end(), frame.bytes.begin(), frame.bytes.end());
  return wire;
}

std::vector<uint8_t> build_elementary_stream(
    CodecType codec, const std::vector<EncodedFrame>& frames) {
  std::vector<uint8_t> stream;
  for (const auto& frame : frames) {
    auto wire = frame_wire_bytes(codec, frame);
    stream.insert(stream.end(), wire.begin(), wire.end());
  }
  return stream;
}

Diagnostic generate_sample(const SampleConfig& config,
                           GeneratedSample* sample) {
  // Transport/audio assembly is added in the next generator batch.
  (void)config;
  (void)sample;
  return diag(Status::UsageError, "sample assembly not implemented yet");
}

Diagnostic write_sample_bundle(const std::string& dir,
                               const GeneratedSample& sample) {
  (void)dir;
  (void)sample;
  return diag(Status::UsageError, "sample bundle writer not implemented yet");
}

}  // namespace media_archive
