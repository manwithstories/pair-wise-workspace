#include "relay/frame_decoder.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/mem.h>
}

namespace relay {

namespace {
std::string av_err(int code) {
  char buf[AV_ERROR_MAX_STRING_SIZE] = {0};
  av_strerror(code, buf, sizeof(buf));
  return buf;
}
} // namespace

DecodedFrameInfo decode_validate(const std::vector<uint8_t>& payload) {
  DecodedFrameInfo info;
  if (payload.empty()) {
    info.error = "empty payload";
    return info;
  }
  const AVCodec* codec = avcodec_find_decoder(AV_CODEC_ID_PNG);
  if (!codec) {
    info.error = "png decoder unavailable";
    return info;
  }
  AVCodecContext* ctx = avcodec_alloc_context3(codec);
  if (!ctx) {
    info.error = "decoder alloc failed";
    return info;
  }
  int ret = avcodec_open2(ctx, codec, nullptr);
  if (ret < 0) {
    info.error = "decoder open: " + av_err(ret);
    avcodec_free_context(&ctx);
    return info;
  }

  AVPacket* pkt = av_packet_alloc();
  AVFrame* frame = av_frame_alloc();
  av_new_packet(pkt, static_cast<int>(payload.size()));
  std::memcpy(pkt->data, payload.data(), payload.size());

  ret = avcodec_send_packet(ctx, pkt);
  if (ret >= 0) ret = avcodec_receive_frame(ctx, frame);
  if (ret < 0) {
    info.error = "decode: " + av_err(ret);
  } else if (frame->width <= 0 || frame->height <= 0) {
    info.error = "invalid dimensions";
  } else {
    info.ok = true;
    info.width = frame->width;
    info.height = frame->height;
  }

  av_frame_free(&frame);
  av_packet_free(&pkt);
  avcodec_free_context(&ctx);
  return info;
}

std::vector<uint8_t> encode_test_pattern_png(int width, int height,
                                             uint64_t seed) {
  const AVCodec* codec = avcodec_find_encoder(AV_CODEC_ID_PNG);
  if (!codec) return {};
  AVCodecContext* ctx = avcodec_alloc_context3(codec);
  if (!ctx) return {};
  ctx->width = width;
  ctx->height = height;
  ctx->pix_fmt = AV_PIX_FMT_RGB24;
  ctx->time_base = {1, 25};
  if (avcodec_open2(ctx, codec, nullptr) < 0) {
    avcodec_free_context(&ctx);
    return {};
  }

  AVFrame* frame = av_frame_alloc();
  frame->format = AV_PIX_FMT_RGB24;
  frame->width = width;
  frame->height = height;
  av_frame_get_buffer(frame, 0);
  av_frame_make_writable(frame);
  // Deterministic gradient pattern keyed by seed.
  for (int y = 0; y < height; ++y) {
    uint8_t* row = frame->data[0] + y * frame->linesize[0];
    for (int x = 0; x < width; ++x) {
      row[x * 3 + 0] = static_cast<uint8_t>((x + seed) & 0xff);
      row[x * 3 + 1] = static_cast<uint8_t>((y + seed * 7) & 0xff);
      row[x * 3 + 2] = static_cast<uint8_t>((x ^ y) + (seed * 13) & 0xff);
    }
  }

  std::vector<uint8_t> out;
  AVPacket* pkt = av_packet_alloc();
  if (avcodec_send_frame(ctx, frame) >= 0) {
    while (avcodec_receive_packet(ctx, pkt) >= 0) {
      out.assign(pkt->data, pkt->data + pkt->size);
      av_packet_unref(pkt);
    }
  }
  av_packet_free(&pkt);
  av_frame_free(&frame);
  avcodec_free_context(&ctx);
  return out;
}

} // namespace relay
