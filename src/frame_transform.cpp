#include "frame_transform.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/error.h>
#include <libavutil/frame.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libavutil/pixfmt.h>
#include <libswscale/swscale.h>
}

#include <algorithm>
#include <cstring>
#include <string>

#include "sha256.hpp"

namespace mfs {
namespace {

constexpr int kPngCompressionLevel = 3;
constexpr uint32_t kMaxDimension = 16384;

AVCodecID to_av_codec(CodecId codec) {
  switch (codec) {
    case CodecId::Png:
      return AV_CODEC_ID_PNG;
    case CodecId::Mjpeg:
      return AV_CODEC_ID_MJPEG;
  }
  return AV_CODEC_ID_NONE;
}

}  // namespace

struct FrameTransform::Impl {
  AVCodec* png_encoder{nullptr};
  bool encoder_ready{false};
  AVCodecContext* encoder_ctx{nullptr};
};

FrameTransform::FrameTransform() : impl_(new Impl()) {}

FrameTransform::~FrameTransform() {
  if (impl_->encoder_ctx != nullptr) {
    avcodec_free_context(&impl_->encoder_ctx);
  }
  delete impl_;
}

std::string ffmpeg_error_string(int errnum) {
  char buffer[AV_ERROR_MAX_STRING_SIZE] = {};
  av_strerror(errnum, buffer, sizeof(buffer));
  return std::string(buffer);
}

DecodeOutcome FrameTransform::decode(CodecId codec,
                                     std::span<const uint8_t> payload,
                                     uint32_t expected_width,
                                     uint32_t expected_height) {
  DecodeOutcome outcome;
  if (payload.empty()) {
    outcome.detail = "empty payload";
    return outcome;
  }
  const AVCodec* decoder = avcodec_find_decoder(to_av_codec(codec));
  if (decoder == nullptr) {
    outcome.detail = std::string("no decoder for ") + to_string(codec);
    return outcome;
  }
  AVCodecContext* ctx = avcodec_alloc_context3(decoder);
  if (ctx == nullptr) {
    outcome.detail = "cannot allocate decoder context";
    return outcome;
  }
  ctx->thread_count = 1;
  ctx->err_recognition = AV_EF_EXPLODE;
  int ret = avcodec_open2(ctx, decoder, nullptr);
  if (ret < 0) {
    outcome.detail = "cannot open decoder: " + ffmpeg_error_string(ret);
    avcodec_free_context(&ctx);
    return outcome;
  }

  AVPacket* packet = av_packet_alloc();
  AVFrame* frame = av_frame_alloc();
  bool decoded = false;
  if (packet != nullptr && frame != nullptr) {
    ret = av_new_packet(packet, static_cast<int>(payload.size()));
    if (ret >= 0) {
      std::memcpy(packet->data, payload.data(), payload.size());
      packet->size = static_cast<int>(payload.size());
      ret = avcodec_send_packet(ctx, packet);
      if (ret >= 0) {
        ret = avcodec_receive_frame(ctx, frame);
        decoded = ret >= 0;
      } else {
        outcome.detail = "decoder rejected packet: " + ffmpeg_error_string(ret);
      }
    } else {
      outcome.detail = "cannot allocate packet";
    }
    if (!decoded && outcome.detail.empty()) {
      outcome.detail = "decoder returned no frame: " + ffmpeg_error_string(ret);
    }
  } else {
    outcome.detail = "cannot allocate decoder buffers";
  }

  if (decoded) {
    const bool geometry_ok = frame->width > 0 && frame->height > 0 &&
                             static_cast<uint32_t>(frame->width) <= kMaxDimension &&
                             static_cast<uint32_t>(frame->height) <= kMaxDimension;
    if (!geometry_ok) {
      outcome.detail = "decoded geometry out of range";
      decoded = false;
    } else if (expected_width != 0 && expected_height != 0 &&
               (static_cast<uint32_t>(frame->width) != expected_width ||
                static_cast<uint32_t>(frame->height) != expected_height)) {
      outcome.detail = "decoded size " + std::to_string(frame->width) + "x" +
                       std::to_string(frame->height) + " differs from declared " +
                       std::to_string(expected_width) + "x" +
                       std::to_string(expected_height);
      decoded = false;
    }
  }

  if (decoded) {
    // Convert every supported layout to packed RGB24 via libswscale. The PNG
    // decoder yields RGB24/RGBA/RGB48BE depending on the source bit depth.
    SwsContext* sws = sws_getContext(
        frame->width, frame->height, static_cast<AVPixelFormat>(frame->format),
        frame->width, frame->height, AV_PIX_FMT_RGB24, SWS_POINT, nullptr,
        nullptr, nullptr);
    if (sws == nullptr) {
      outcome.detail = "cannot create colour converter";
      decoded = false;
    } else {
      const int linesize = frame->width * 3;
      std::vector<uint8_t> rgb(static_cast<size_t>(linesize) * frame->height);
      uint8_t* dst[4] = {rgb.data(), nullptr, nullptr, nullptr};
      const int dst_linesize[4] = {linesize, 0, 0, 0};
      const int scaled = sws_scale(sws, frame->data, frame->linesize, 0,
                                   frame->height, dst, dst_linesize);
      sws_freeContext(sws);
      if (scaled <= 0) {
        outcome.detail = "colour conversion failed";
        decoded = false;
      } else {
        outcome.width = static_cast<uint32_t>(frame->width);
        outcome.height = static_cast<uint32_t>(frame->height);
        outcome.content_sha256 = Sha256::hash(rgb);
        outcome.ok = true;
      }
    }
  }

  av_frame_free(&frame);
  av_packet_free(&packet);
  avcodec_free_context(&ctx);
  return outcome;
}

bool FrameTransform::encode_png(std::span<const uint8_t> rgb24, uint32_t width,
                                uint32_t height, std::vector<uint8_t>& out) {
  out.clear();
  if (width == 0 || height == 0 || width > kMaxDimension ||
      height > kMaxDimension) {
    return false;
  }
  const size_t expected = static_cast<size_t>(width) * height * 3;
  if (rgb24.size() != expected) {
    return false;
  }
  if (!impl_->encoder_ready) {
    impl_->png_encoder = const_cast<AVCodec*>(
        avcodec_find_encoder(AV_CODEC_ID_PNG));
    if (impl_->png_encoder == nullptr) {
      return false;
    }
    impl_->encoder_ready = true;
  }
  AVCodecContext* ctx = avcodec_alloc_context3(impl_->png_encoder);
  if (ctx == nullptr) {
    return false;
  }
  ctx->width = static_cast<int>(width);
  ctx->height = static_cast<int>(height);
  ctx->pix_fmt = AV_PIX_FMT_RGB24;
  ctx->time_base = AVRational{1, 1000};
  ctx->thread_count = 1;
  av_opt_set_int(ctx->priv_data, "compression_level", kPngCompressionLevel, 0);
  if (avcodec_open2(ctx, impl_->png_encoder, nullptr) < 0) {
    avcodec_free_context(&ctx);
    return false;
  }

  AVFrame* frame = av_frame_alloc();
  AVPacket* packet = av_packet_alloc();
  bool ok = false;
  if (frame != nullptr && packet != nullptr) {
    frame->format = AV_PIX_FMT_RGB24;
    frame->width = static_cast<int>(width);
    frame->height = static_cast<int>(height);
    if (av_frame_get_buffer(frame, 32) >= 0) {
      for (uint32_t y = 0; y < height; ++y) {
        std::memcpy(frame->data[0] + static_cast<ptrdiff_t>(y) * frame->linesize[0],
                    rgb24.data() + static_cast<size_t>(y) * width * 3,
                    static_cast<size_t>(width) * 3);
      }
      frame->pts = 0;
      if (avcodec_send_frame(ctx, frame) >= 0 &&
          avcodec_receive_packet(ctx, packet) >= 0) {
        out.assign(packet->data, packet->data + packet->size);
        ok = true;
      }
    }
  }
  av_packet_free(&packet);
  av_frame_free(&frame);
  avcodec_free_context(&ctx);
  return ok;
}

}  // namespace mfs
