#include "media.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <memory>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
}

namespace mb {

namespace {
struct CtxDel {
  void operator()(AVFormatContext* c) const { if (c) avformat_close_input(&c); }
};
using InPtr = std::unique_ptr<AVFormatContext, CtxDel>;
struct CodecDel {
  void operator()(AVCodecContext* c) const { if (c) avcodec_free_context(&c); }
};
using CodecPtr = std::unique_ptr<AVCodecContext, CodecDel>;
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

// 与 media_make 保持一致的确定性参考亮度（同公式）。
int reference_luma(int idx, int64_t seed) {
  return 40 + (idx * 7 + seed) % 180;
}

// 计算一帧 Y 平面相对参考常量亮度的 PSNR（dB）。
double frame_psnr(const AVFrame* f, int ref) {
  double mse = 0.0;
  int count = 0;
  for (int y = 0; y < f->height; ++y) {
    const uint8_t* row = f->data[0] + y * f->linesize[0];
    for (int x = 0; x < f->width; ++x) {
      double d = double(row[x]) - double(ref);
      mse += d * d;
      ++count;
    }
  }
  mse /= count;
  if (mse < 1e-9) return 99.0;
  return 10.0 * std::log10((255.0 * 255.0) / mse);
}

bool open_input(const std::string& path, AVFormatContext** out) {
  AVFormatContext* c = nullptr;
  if (avformat_open_input(&c, path.c_str(), nullptr, nullptr) < 0) return false;
  if (avformat_find_stream_info(c, nullptr) < 0) {
    avformat_close_input(&c);
    return false;
  }
  *out = c;
  return true;
}
}  // namespace

bool probe_media(const std::string& path, const MediaSpec& spec,
                 MediaStats* st, bool tolerate_errors, ErrList* errs) {
  AVFormatContext* raw = nullptr;
  if (!open_input(path, &raw)) {
    if (errs) errs->push_back({kMediaOpen, "cannot open input", -1});
    return false;
  }
  InPtr in(raw);
  int vi = av_find_best_stream(in.get(), AVMEDIA_TYPE_VIDEO, -1, -1,
                               nullptr, 0);
  int ai = av_find_best_stream(in.get(), AVMEDIA_TYPE_AUDIO, -1, -1,
                               nullptr, 0);
  if (vi < 0) {
    st->missing_tracks++;
    if (errs) errs->push_back({kMediaMissing, "no video track", -1});
    return false;
  }
  if (ai < 0 && spec.include_audio) {
    st->missing_tracks++;
    if (errs) errs->push_back({kMediaMissing, "audio track missing", -1});
  }

  CodecPtr vdec;
  const AVCodec* vcodec = avcodec_find_decoder(
      in->streams[vi]->codecpar->codec_id);
  vdec.reset(avcodec_alloc_context3(vcodec));
  avcodec_parameters_to_context(vdec.get(), in->streams[vi]->codecpar);
  if (avcodec_open2(vdec.get(), vcodec, nullptr) < 0) return false;

  CodecPtr adec;
  SwrPtr swr;
  if (ai >= 0) {
    const AVCodec* ac = avcodec_find_decoder(
        in->streams[ai]->codecpar->codec_id);
    adec.reset(avcodec_alloc_context3(ac));
    avcodec_parameters_to_context(adec.get(), in->streams[ai]->codecpar);
    avcodec_open2(adec.get(), ac, nullptr);
    SwrContext* s = nullptr;
    AVChannelLayout mono;
    av_channel_layout_default(&mono, 1);
    swr_alloc_set_opts2(&s, &mono, AV_SAMPLE_FMT_FLT, spec.audio_sample_rate,
                        &adec->ch_layout, adec->sample_fmt,
                        adec->sample_rate, 0, nullptr);
    swr.reset(s);
    if (swr) swr_init(swr.get());
  }

  double psnr_sum = 0;
  float peak = 0.0f;
  PktPtr pkt(av_packet_alloc());
  FramePtr frame(av_frame_alloc());
  int vidx = 0;
  while (av_read_frame(in.get(), pkt.get()) >= 0) {
    AVCodecContext* dec = pkt->stream_index == vi ? vdec.get()
                          : (ai >= 0 && pkt->stream_index == ai ? adec.get()
                                                                : nullptr);
    if (!dec) { av_packet_unref(pkt.get()); continue; }
    int sr = avcodec_send_packet(dec, pkt.get());
    av_packet_unref(pkt.get());
    if (sr < 0) {
      st->decode_errors++;
      if (errs) errs->push_back({kMediaDecode, "send_packet failed", -1});
      if (!tolerate_errors) return false;
      continue;
    }
    while (true) {
      int rr = avcodec_receive_frame(dec, frame.get());
      if (rr == AVERROR(EAGAIN) || rr == AVERROR_EOF) break;
      if (rr < 0) {
        st->decode_errors++;
        if (errs) errs->push_back({kMediaDecode, "receive_frame failed", -1});
        if (!tolerate_errors) return false;
        break;
      }
      if (dec == vdec.get()) {
        psnr_sum += frame_psnr(frame.get(),
                               reference_luma(vidx, spec.corrupt_seed));
        st->video_frames++;
        vidx++;
      } else if (swr) {
        st->audio_frames++;
        float buf[4096];
        uint8_t* outp = reinterpret_cast<uint8_t*>(buf);
        int got = swr_convert(swr.get(), &outp, 4096,
                              const_cast<const uint8_t**>(frame->data),
                              frame->nb_samples);
        for (int k = 0; k < got && k < 4096; ++k)
          peak = std::max(peak, std::fabs(buf[k]));
      }
      av_frame_unref(frame.get());
    }
  }
  // flush 解码器，捞取缓存帧。
  avcodec_send_packet(vdec.get(), nullptr);
  while (avcodec_receive_frame(vdec.get(), frame.get()) == 0) {
    psnr_sum += frame_psnr(frame.get(),
                           reference_luma(vidx, spec.corrupt_seed));
    st->video_frames++;
    vidx++;
    av_frame_unref(frame.get());
  }

  if (st->video_frames > 0) st->video_psnr_db = psnr_sum / st->video_frames;
  st->audio_peak_dbfs = peak > 0 ? 20.0 * std::log10(peak) : -99.0;
  if (in->duration > 0) st->duration_ms = in->duration / 1000;
  return st->decode_errors == 0 || tolerate_errors;
}

bool transcode_media(const std::string& in_path, const std::string& out_path,
                     const MediaSpec& spec, int64_t target_bitrate,
                     const std::string& container, TranscodeResult* out,
                     ErrList* errs) {
  AVFormatContext* raw = nullptr;
  if (!open_input(in_path, &raw)) {
    if (errs) errs->push_back({kMediaOpen, "cannot open input", -1});
    return false;
  }
  InPtr in(raw);
  int vi = av_find_best_stream(in.get(), AVMEDIA_TYPE_VIDEO, -1, -1, nullptr, 0);
  int ai = av_find_best_stream(in.get(), AVMEDIA_TYPE_AUDIO, -1, -1, nullptr, 0);
  (void)ai;  // 再封装当前仅处理视频轨；缺音轨由 probe 阶段统计
  if (vi < 0) {
    if (errs) errs->push_back({kMediaMissing, "no video track", -1});
    out->stats.missing_tracks++;
    return false;
  }

  AVFormatContext* ofmt = nullptr;
  if (avformat_alloc_output_context2(&ofmt, nullptr, container.c_str(),
                                     out_path.c_str()) < 0) {
    if (errs) errs->push_back({kMediaWrite, "alloc output failed", -1});
    return false;
  }
  // 输出 context 的释放器：关 IO 后 free（不能用输入专用的 close_input）。
  struct OutDel {
    void operator()(AVFormatContext* c) const {
      if (c) {
        if (c->pb) avio_closep(&c->pb);
        avformat_free_context(c);
      }
    }
  };
  std::unique_ptr<AVFormatContext, OutDel> outg(ofmt);

  const AVCodec* vcodec = avcodec_find_encoder_by_name("libx264");
  CodecPtr venc(avcodec_alloc_context3(vcodec));
  venc->width = spec.width;
  venc->height = spec.height;
  venc->pix_fmt = AV_PIX_FMT_YUV420P;
  venc->time_base = AVRational{int(spec.fps.den), int(spec.fps.num)};
  venc->framerate = AVRational{int(spec.fps.num), int(spec.fps.den)};
  venc->bit_rate = target_bitrate;
  venc->rc_buffer_size = target_bitrate / 2;
  venc->rc_max_rate = target_bitrate;
  av_opt_set(venc->priv_data, "preset", "veryfast", 0);
  av_opt_set(venc->priv_data, "tune", "zerolatency", 0);
  av_opt_set(venc->priv_data, "nal-hrd", "cbr", 0);
  ofmt->strict_std_compliance = FF_COMPLIANCE_EXPERIMENTAL;
  if (ofmt->oformat->flags & AVFMT_GLOBALHEADER)
    venc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
  if (avcodec_open2(venc.get(), vcodec, nullptr) < 0) return false;
  AVStream* ov = avformat_new_stream(outg.get(), nullptr);
  avcodec_parameters_from_context(ov->codecpar, venc.get());
  ov->time_base = venc->time_base;

  if (!(outg->oformat->flags & AVFMT_NOFILE) &&
      avio_open(&outg->pb, out_path.c_str(), AVIO_FLAG_WRITE) < 0) {
    if (errs) errs->push_back({kMediaWrite, "avio_open output failed", -1});
    return false;
  }
  if (avformat_write_header(outg.get(), nullptr) < 0) {
    if (errs) errs->push_back({kMediaWrite, "write header failed", -1});
    return false;
  }

  CodecPtr vdec;
  const AVCodec* dc = avcodec_find_decoder(in->streams[vi]->codecpar->codec_id);
  vdec.reset(avcodec_alloc_context3(dc));
  avcodec_parameters_to_context(vdec.get(), in->streams[vi]->codecpar);
  avcodec_open2(vdec.get(), dc, nullptr);

  PktPtr pkt(av_packet_alloc());
  FramePtr frame(av_frame_alloc()), encf(av_frame_alloc());
  int64_t frame_no = 0, fail_start = -1;
  auto write_enc = [&](AVFrame* f) {
    if (avcodec_send_frame(venc.get(), f) < 0) return;
    while (avcodec_receive_packet(venc.get(), pkt.get()) == 0) {
      av_packet_rescale_ts(pkt.get(), venc->time_base, ov->time_base);
      pkt->stream_index = ov->index;
      av_interleaved_write_frame(outg.get(), pkt.get());
      av_packet_unref(pkt.get());
    }
  };
  while (av_read_frame(in.get(), pkt.get()) >= 0) {
    if (pkt->stream_index != vi) { av_packet_unref(pkt.get()); continue; }
    int sr = avcodec_send_packet(vdec.get(), pkt.get());
    av_packet_unref(pkt.get());
    if (sr < 0) {
      out->stats.decode_errors++;
      if (fail_start < 0) fail_start = frame_no;
      continue;
    }
    while (avcodec_receive_frame(vdec.get(), frame.get()) == 0) {
      frame->pts = frame_no++;
      write_enc(frame.get());
      av_frame_unref(frame.get());
    }
  }
  write_enc(nullptr);
  av_write_trailer(outg.get());

  if (fail_start >= 0) {
    int64_t tick = kMasterHz * spec.fps.den / spec.fps.num;
    out->failed_ranges.emplace_back(fail_start * tick, frame_no * tick);
  }
  out->output_bytes = mb::file_size(out_path);
  double dur_s = double(spec.duration_ms) / 1000.0;
  out->stats.actual_video_bitrate_bps =
      dur_s > 0 ? double(out->output_bytes) * 8.0 / dur_s : 0.0;
  out->stats.video_frames = frame_no;
  out->ok = true;
  return true;
}

}  // namespace mb
