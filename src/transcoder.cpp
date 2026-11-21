#include "media/transcoder.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
}

#include <algorithm>
#include <cmath>
#include <cstring>
#include <filesystem>

namespace media {

struct Transcoder::Impl {
  TranscodeConfig cfg;
  AudioFormat srcFmt;
  AVFormatContext* oc = nullptr;
  AVCodecContext* enc = nullptr;
  SwrContext* swr = nullptr;
  AVStream* stream = nullptr;
  AVFrame* inFrame = nullptr;
  AVFrame* outFrame = nullptr;
  AVPacket* pkt = nullptr;
  uint32_t outRate = 0;
  uint16_t outChannels = 0;
  AVChannelLayout outLayout{};
  SampleTick framesIn = 0;
  SampleTick framesOut = 0;
  uint64_t payloadBytes = 0;
  bool opened = false;

  MediaError fail(ErrorCode code, const std::string& msg) {
    return MediaError{code, Stage::Transcode, cfg.outputPath, -1, framesIn,
                      msg, false};
  }
};

namespace {
AVChannelLayout defaultLayout(uint16_t channels) {
  AVChannelLayout layout{};
  av_channel_layout_default(&layout, channels);
  return layout;
}
}  // namespace

Transcoder::Transcoder(TranscodeConfig cfg, AudioFormat sourceFormat)
    : impl_(new Impl) {
  impl_->cfg = std::move(cfg);
  impl_->srcFmt = sourceFormat;
}

Transcoder::~Transcoder() {
  if (impl_->swr) swr_free(&impl_->swr);
  if (impl_->enc) avcodec_free_context(&impl_->enc);
  if (impl_->oc) {
    if (impl_->opened) av_write_trailer(impl_->oc);
    avformat_free_context(impl_->oc);
  }
  if (impl_->inFrame) av_frame_free(&impl_->inFrame);
  if (impl_->outFrame) av_frame_free(&impl_->outFrame);
  if (impl_->pkt) av_packet_free(&impl_->pkt);
  av_channel_layout_uninit(&impl_->outLayout);
  delete impl_;
}

SampleTick Transcoder::encodedFrames() const { return impl_->framesOut; }

uint32_t Transcoder::outputSampleRate() const { return impl_->outRate; }

MediaError Transcoder::open() {
  const char* path = impl_->cfg.outputPath.c_str();
  // 仅用文件名推断容器格式，避免目录前缀干扰扩展名探测。
  std::string base = std::filesystem::path(path).filename().string();
  if (avformat_alloc_output_context2(&impl_->oc, nullptr, nullptr,
                                     base.c_str()) < 0 ||
      !impl_->oc)
    return impl_->fail(ErrorCode::EncodeFailed,
                       "cannot infer output format from extension");
  const AVCodec* codec = nullptr;
  // 原生 AAC 在短素材上 VBR 偏离目标码率；macOS 优先 AudioToolbox aac_at，
  // 其码率控制更贴合目标，找不到时回退原生 aac。
  if (impl_->oc->oformat->audio_codec == AV_CODEC_ID_AAC)
    codec = avcodec_find_encoder_by_name("aac_at");
  if (!codec)
    codec = avcodec_find_encoder(impl_->oc->oformat->audio_codec);
  if (!codec)
    return impl_->fail(ErrorCode::UnsupportedFormat,
                       "no encoder available for container");
  impl_->enc = avcodec_alloc_context3(codec);
  impl_->outRate = impl_->cfg.targetSampleRate
                       ? impl_->cfg.targetSampleRate
                       : impl_->srcFmt.sampleRate;
  impl_->outChannels = impl_->cfg.targetChannels
                           ? impl_->cfg.targetChannels
                           : impl_->srcFmt.channels;
  impl_->outLayout = defaultLayout(impl_->outChannels);
  impl_->enc->sample_rate = impl_->outRate;
  impl_->enc->ch_layout = impl_->outLayout;
  impl_->enc->bit_rate = impl_->cfg.targetBitrate;
  const AVSampleFormat* fmts = nullptr;
  int nFmts = 0;
  if (avcodec_get_supported_config(impl_->enc, codec,
                                   AV_CODEC_CONFIG_SAMPLE_FORMAT, 0,
                                   reinterpret_cast<const void**>(&fmts),
                                   &nFmts) >= 0 &&
      nFmts > 0)
    impl_->enc->sample_fmt = fmts[0];
  else
    impl_->enc->sample_fmt = AV_SAMPLE_FMT_FLTP;
  if (impl_->oc->oformat->flags & AVFMT_GLOBALHEADER)
    impl_->enc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
  if (avcodec_open2(impl_->enc, codec, nullptr) < 0)
    return impl_->fail(ErrorCode::EncodeFailed, "avcodec_open2 failed");

  impl_->stream = avformat_new_stream(impl_->oc, nullptr);
  if (!impl_->stream)
    return impl_->fail(ErrorCode::EncodeFailed, "avformat_new_stream failed");
  if (avcodec_parameters_from_context(impl_->stream->codecpar,
                                      impl_->enc) < 0)
    return impl_->fail(ErrorCode::EncodeFailed,
                       "avcodec_parameters_from_context failed");

  if (!(impl_->oc->oformat->flags & AVFMT_NOFILE)) {
    if (avio_open(&impl_->oc->pb, path, AVIO_FLAG_WRITE) < 0)
      return impl_->fail(ErrorCode::IoFailure, "avio_open failed");
  }
  if (avformat_write_header(impl_->oc, nullptr) < 0)
    return impl_->fail(ErrorCode::EncodeFailed, "avformat_write_header failed");
  impl_->opened = true;

  AVChannelLayout srcLayout = defaultLayout(impl_->srcFmt.channels);
  if (swr_alloc_set_opts2(&impl_->swr, &impl_->outLayout,
                          AV_SAMPLE_FMT_FLTP, impl_->outRate, &srcLayout,
                          AV_SAMPLE_FMT_S32, impl_->srcFmt.sampleRate, 0,
                          nullptr) < 0) {
    av_channel_layout_uninit(&srcLayout);
    return impl_->fail(ErrorCode::EncodeFailed, "swr_alloc_set_opts2 failed");
  }
  av_channel_layout_uninit(&srcLayout);
  if (swr_init(impl_->swr) < 0)
    return impl_->fail(ErrorCode::EncodeFailed, "swr_init failed");

  impl_->inFrame = av_frame_alloc();
  impl_->outFrame = av_frame_alloc();
  impl_->pkt = av_packet_alloc();
  impl_->outRate = static_cast<uint32_t>(impl_->enc->sample_rate);
  return MediaError{};
}

MediaError Transcoder::push(const TranscodeInput& in) {
  if (!impl_->opened) return impl_->fail(ErrorCode::EncodeFailed, "not open");
  impl_->framesIn += static_cast<SampleTick>(in.frames);

  AVChannelLayout inLayout = defaultLayout(in.channels);
  // AAC 等编码器要求固定 frame_size；按编码器帧长切块喂入。
  int chunk = impl_->enc->frame_size > 0 ? impl_->enc->frame_size
                                         : static_cast<int>(in.frames);
  size_t offset = 0;
  while (offset < in.frames) {
    int n = std::min<int>(chunk, static_cast<int>(in.frames - offset));
    av_frame_unref(impl_->inFrame);
    impl_->inFrame->sample_rate = in.sampleRate;
    impl_->inFrame->format = AV_SAMPLE_FMT_S32;
    av_channel_layout_copy(&impl_->inFrame->ch_layout, &inLayout);
    impl_->inFrame->nb_samples = n;
    if (av_frame_get_buffer(impl_->inFrame, 0) < 0)
      return impl_->fail(ErrorCode::EncodeFailed, "in frame alloc failed");
    std::memcpy(impl_->inFrame->data[0],
                in.samples + offset * in.channels,
                n * in.channels * sizeof(int32_t));

    int dstCap = swr_get_out_samples(impl_->swr, n);
    av_frame_unref(impl_->outFrame);
    if (av_frame_copy_props(impl_->outFrame, impl_->inFrame) < 0)
      return impl_->fail(ErrorCode::EncodeFailed, "frame props failed");
    impl_->outFrame->format = AV_SAMPLE_FMT_FLTP;
    av_channel_layout_copy(&impl_->outFrame->ch_layout, &impl_->outLayout);
    impl_->outFrame->sample_rate = impl_->enc->sample_rate;
    impl_->outFrame->nb_samples = dstCap;
    if (av_frame_get_buffer(impl_->outFrame, 0) < 0)
      return impl_->fail(ErrorCode::EncodeFailed, "out frame alloc failed");

    uint8_t* outPtrs[8] = {};
    for (int i = 0; i < 8; ++i) outPtrs[i] = impl_->outFrame->data[i];
    int converted = swr_convert(impl_->swr, outPtrs, dstCap,
                                impl_->inFrame->data, n);
    if (converted < 0)
      return impl_->fail(ErrorCode::EncodeFailed, "swr_convert failed");
    impl_->outFrame->nb_samples = converted;
    impl_->outFrame->pts = impl_->framesOut;
    impl_->framesOut += converted;

    if (avcodec_send_frame(impl_->enc, impl_->outFrame) < 0)
      return impl_->fail(ErrorCode::EncodeFailed, "send_frame failed");
    while (true) {
      int rc = avcodec_receive_packet(impl_->enc, impl_->pkt);
      if (rc == AVERROR(EAGAIN) || rc == AVERROR_EOF) break;
      if (rc < 0)
        return impl_->fail(ErrorCode::EncodeFailed, "receive_packet failed");
      impl_->payloadBytes += impl_->pkt->size;
      av_packet_rescale_ts(impl_->pkt, impl_->enc->time_base,
                           impl_->stream->time_base);
      impl_->pkt->stream_index = impl_->stream->index;
      if (av_interleaved_write_frame(impl_->oc, impl_->pkt) < 0)
        return impl_->fail(ErrorCode::EncodeFailed, "write_frame failed");
    }
    offset += static_cast<size_t>(n);
  }
  av_channel_layout_uninit(&inLayout);
  return MediaError{};
}

MediaError Transcoder::finish(TranscodeResult* result) {
  if (avcodec_send_frame(impl_->enc, nullptr) < 0)
    return impl_->fail(ErrorCode::EncodeFailed, "flush send failed");
  while (true) {
    int rc = avcodec_receive_packet(impl_->enc, impl_->pkt);
    if (rc == AVERROR_EOF) break;
    if (rc < 0)
      return impl_->fail(ErrorCode::EncodeFailed, "flush receive failed");
    impl_->payloadBytes += impl_->pkt->size;
    av_packet_rescale_ts(impl_->pkt, impl_->enc->time_base,
                         impl_->stream->time_base);
    impl_->pkt->stream_index = impl_->stream->index;
    if (av_interleaved_write_frame(impl_->oc, impl_->pkt) < 0)
      return impl_->fail(ErrorCode::EncodeFailed, "flush write failed");
  }
  if (av_write_trailer(impl_->oc) < 0)
    return impl_->fail(ErrorCode::EncodeFailed, "write_trailer failed");
  impl_->opened = false;
  if (!(impl_->oc->oformat->flags & AVFMT_NOFILE))
    avio_closep(&impl_->oc->pb);

  std::error_code ec;
  uint64_t fileBytes =
      std::filesystem::file_size(impl_->cfg.outputPath, ec);
  if (ec) return impl_->fail(ErrorCode::IoFailure, "output stat failed");

  // 实测码率按输出文件整体字节与整数帧时长推导（与 ffprobe 同口径），
  // 全程整数运算，无浮点累计漂移。
  result->targetBitrateBps = impl_->cfg.targetBitrate;
  result->outputFrames = impl_->framesOut;
  result->outputSampleRate = impl_->outRate;
  result->outputChannels = impl_->outChannels;
  result->outputBytes = fileBytes;
  result->actualBitrateBps =
      impl_->framesOut > 0
          ? static_cast<int64_t>(
                static_cast<__int128_t>(fileBytes) * 8 * impl_->outRate /
                impl_->framesOut)
          : 0;
  return MediaError{};
}

}  // namespace media
