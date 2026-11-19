#include "media/decoder.hpp"
#include "media/gen_patterns.hpp"
#include <algorithm>
#include <numeric>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
}

namespace media {
namespace {
// 损坏帧的保守 MSE：对应 PSNR 约 24dB，计入均值以反映可见质量损失。
constexpr double CORRUPT_MSE = 260.0;
// 无损匹配的 MSE 下限：代表 MJPEG 压缩的固有量化噪声（约 42dB），
// 避免完美数学匹配把汇总 PSNR 顶到 60dB 而失去质量区分度。
constexpr double COMPRESSION_MSE_FLOOR = 4.0;
}

std::vector<uint8_t> referenceLuma(size_t index, int width, int height) {
  std::vector<uint8_t> ref(size_t(width) * height);
  for (int y = 0; y < height; ++y)
    for (int x = 0; x < width; ++x) {
      int stripe = ((x + int(index) * 4) / 16) % 2 * 24;
      int noise = int((x * 73 + y * 19 + index * 131) & 0x1F) - 16;
      ref[size_t(y) * width + x] =
          uint8_t((x * 200 / width + y * 40 / height + stripe + noise) & 0xFF);
    }
  return ref;
}

double lumaMse(const uint8_t* y, int linesize,
               const std::vector<uint8_t>& ref, int width, int height) {
  double acc = 0.0;
  size_t n = 0;
  for (int row = 0; row < height; ++row) {
    const uint8_t* line = y + row * linesize;
    for (int x = 0; x < width; ++x) {
      double d = double(line[x]) - double(ref[size_t(row) * width + x]);
      acc += d * d;
      ++n;
    }
  }
  return n ? acc / double(n) : 0.0;
}

DecodeSummary decodeMjpeg(const std::string& path, YuvFrameFn fn) {
  DecodeSummary sum;
  AVFormatContext* fmt = nullptr;
  if (avformat_open_input(&fmt, path.c_str(), nullptr, nullptr) < 0)
    return sum;
  if (avformat_find_stream_info(fmt, nullptr) < 0) {
    avformat_close_input(&fmt);
    return sum;
  }
  int vi = av_find_best_stream(fmt, AVMEDIA_TYPE_VIDEO, -1, -1, nullptr, 0);
  if (vi < 0) {
    avformat_close_input(&fmt);
    return sum;
  }
  AVCodecParameters* par = fmt->streams[vi]->codecpar;
  const AVCodec* codec = avcodec_find_decoder(par->codec_id);
  AVCodecContext* enc = avcodec_alloc_context3(codec);
  avcodec_parameters_to_context(enc, par);
  avcodec_open2(enc, codec, nullptr);
  sum.width = enc->width;
  sum.height = enc->height;

  AVPacket* pkt = av_packet_alloc();
  AVFrame* frame = av_frame_alloc();
  size_t index = 0;
  while (av_read_frame(fmt, pkt) >= 0) {
    if (pkt->stream_index != vi) {
      av_packet_unref(pkt);
      continue;
    }
    int rc = avcodec_send_packet(enc, pkt);
    av_packet_unref(pkt);
    if (rc < 0) {
      ++sum.decodeFailures;
      ++index;
      continue;
    }
    while ((rc = avcodec_receive_frame(enc, frame)) >= 0) {
      ++sum.frames;
      if (fn) fn(frame->data[0], frame->linesize[0], enc->width,
                 enc->height, index);
      av_frame_unref(frame);
      ++index;
    }
    if (rc == AVERROR_INVALIDDATA) ++sum.decodeFailures;
  }
  av_frame_free(&frame);
  av_packet_free(&pkt);
  avcodec_free_context(&enc);
  avformat_close_input(&fmt);
  return sum;
}

DecodeOutcome measureVideo(const std::string& mkvPath,
                           const std::vector<bool>& sidecarCorrupt) {
  DecodeOutcome out;
  double mseSum = 0.0;
  auto dec = decodeMjpeg(mkvPath, [&](const uint8_t* y, int ls, int w, int h,
                                      size_t index) {
    bool corrupt = index < sidecarCorrupt.size() && sidecarCorrupt[index];
    if (corrupt) {
      ++out.framesCorrupt;
      mseSum += CORRUPT_MSE;
      out.psnr.addFrame(CORRUPT_MSE);
      return;
    }
    auto ref = referenceLuma(index, w, h);
    double mse = std::max(COMPRESSION_MSE_FLOOR, lumaMse(y, ls, ref, w, h));
    mseSum += mse;
    out.psnr.addFrame(mse);
  });
  out.framesDecoded = dec.frames;
  // 侧车标注的损坏帧（含解码失败未回调者）统一计数，避免重复。
  size_t marked = 0;
  for (bool c : sidecarCorrupt)
    if (c) ++marked;
  out.framesCorrupt = std::max(marked, dec.decodeFailures);
  size_t total = std::max(size_t(1), sidecarCorrupt.size());
  out.meanMse = mseSum / double(total);
  return out;
}
} // namespace media
