#include "render.hpp"

#include <cstdio>
#include <cstring>
#include <fstream>
#include <memory>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/opt.h>
}

namespace mb {

namespace {
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

// 确定性 RGB 内容：整幅常量灰度，灰度随源帧号周期变化（与媒体参考一致风格）。
void fill_rgb(AVFrame* f, int frame_index, int64_t seed) {
  int gray = 40 + (frame_index * 7 + seed) % 180;
  for (int y = 0; y < f->height; ++y) {
    uint8_t* row = f->data[0] + y * f->linesize[0];
    for (int x = 0; x < f->width; ++x) {
      row[x * 3 + 0] = static_cast<uint8_t>(gray);
      row[x * 3 + 1] = static_cast<uint8_t>(gray);
      row[x * 3 + 2] = static_cast<uint8_t>(gray);
    }
  }
}

// 编码一帧为 PNG 字节。
bool encode_png(AVCodecContext* enc, AVFrame* frame,
                std::vector<uint8_t>* out, std::string* err) {
  PktPtr pkt(av_packet_alloc());
  if (avcodec_send_frame(enc, frame) < 0) {
    if (err) *err = "png send_frame failed";
    return false;
  }
  int rr = avcodec_receive_packet(enc, pkt.get());
  if (rr < 0) { if (err) *err = "png receive_packet failed"; return false; }
  out->assign(pkt->data, pkt->data + pkt->size);
  return true;
}

// 在已排序、互不重叠的时间线区段中定位覆盖 tick 的片段。
const PlacedSegment* segment_at(const Timeline& tl, int64_t tick) {
  for (const auto& s : tl.segments)
    if (tick >= s.start && tick < s.end) return &s;
  return nullptr;
}
}  // namespace

bool render_frames(const RenderConfig& cfg, RenderResult* out, ErrList* errs) {
  std::string ioerr;
  if (!ensure_dir(cfg.out_dir, &ioerr)) {
    if (errs) errs->push_back({kRenderWrite, ioerr, -1});
    return false;
  }
  const auto& spec = cfg.media;
  const int64_t frame_tick = kMasterHz * spec.fps.den / spec.fps.num;
  const int64_t total_media_frames =
      spec.duration_ms * spec.fps.num / (1000 * spec.fps.den);

  const AVCodec* pngc = avcodec_find_encoder(AV_CODEC_ID_PNG);
  if (!pngc) { if (errs) errs->push_back({kRenderWrite, "no png encoder", -1});
    return false; }
  CodecPtr enc(avcodec_alloc_context3(pngc));
  enc->width = spec.width;
  enc->height = spec.height;
  enc->pix_fmt = AV_PIX_FMT_RGB24;
  enc->time_base = AVRational{1, 1};
  if (avcodec_open2(enc.get(), pngc, nullptr) < 0) {
    if (errs) errs->push_back({kRenderWrite, "open png encoder failed", -1});
    return false;
  }
  FramePtr frame(av_frame_alloc());
  frame->format = AV_PIX_FMT_RGB24;
  frame->width = spec.width;
  frame->height = spec.height;
  if (av_frame_get_buffer(frame.get(), 0) < 0) return false;

  // 决定采样区间与数量。
  int64_t span_start = 0, span_end = 0;
  if (cfg.timeline && !cfg.timeline->segments.empty()) {
    span_start = cfg.timeline->start;
    span_end = cfg.timeline->end;
  } else {
    span_end = total_media_frames * frame_tick;
  }
  int64_t span = span_end - span_start;
  int64_t whole = span > 0 ? span / frame_tick : 0;
  if (span > 0 && span % frame_tick != 0) out->partial_tail = 1;
  int64_t want = cfg.emit_frames > 0 ? cfg.emit_frames : whole;

  std::filesystem::path manifest = cfg.out_dir / "manifest.csv";
  std::ofstream mf(manifest, std::ios::trunc);
  mf << "file,master_ticks,frame_index,source_offset,track_id,sha256\n";

  int64_t prev_src = -1;
  for (int64_t k = 0; k < want; ++k) {
    int64_t ticks = span_start + k * frame_tick;
    int64_t src = master_to_frame(ticks, Timebase::fps(spec.fps.num,
                                                       spec.fps.den));
    const PlacedSegment* seg =
        cfg.timeline ? segment_at(*cfg.timeline, ticks) : nullptr;
    if (cfg.timeline && !seg) {
      // 采样点落在时间线空隙：明确记为缺帧，不产出 PNG。
      out->missing++;
      if (errs) errs->push_back({kRenderMissing,
          "sample in timeline gap at tick " + std::to_string(ticks), ticks});
      continue;
    }
    if (src >= total_media_frames) {
      out->missing++;
      if (errs) errs->push_back({kRenderMissing,
          "source frame beyond media length", src});
      continue;
    }
    if (src == prev_src) {
      out->duplicates++;
      if (errs) errs->push_back({kRenderDuplicate,
          "repeated sampling of source frame " + std::to_string(src), src});
    }
    prev_src = src;

    if (av_frame_make_writable(frame.get()) < 0) return false;
    fill_rgb(frame.get(), static_cast<int>(src), spec.corrupt_seed);
    frame->pts = k;
    std::vector<uint8_t> png;
    std::string perr;
    if (!encode_png(enc.get(), frame.get(), &png, &perr)) {
      if (errs) errs->push_back({kRenderWrite, perr, src});
      return false;
    }
    char fname[32];
    std::snprintf(fname, sizeof(fname), "frame_%06lld.png",
                  static_cast<long long>(k));
    std::filesystem::path fpath = cfg.out_dir / fname;
    std::ofstream pf(fpath, std::ios::binary | std::ios::trunc);
    pf.write(reinterpret_cast<const char*>(png.data()),
             static_cast<std::streamsize>(png.size()));
    if (!pf) {
      if (errs) errs->push_back({kRenderWrite, "png write failed", src});
      return false;
    }
    std::string hash = sha256_hex(png.data(), png.size());
    RenderFrame rf;
    rf.file = fname;
    rf.master_ticks = ticks;
    rf.frame_index = src;
    rf.source_offset = seg ? seg->source_offset : -1;
    rf.track_id = seg ? seg->track_id : std::string("media");
    rf.sha256 = hash;
    mf << rf.file << ',' << rf.master_ticks << ',' << rf.frame_index << ','
       << rf.source_offset << ',' << rf.track_id << ',' << rf.sha256 << '\n';
    out->frames.push_back(std::move(rf));
    out->exported++;
  }
  mf.close();
  out->manifest_path = manifest;
  if (out->partial_tail && errs)
    errs->push_back({kRenderPartial,
        "final interval shorter than a whole frame; remainder not sampled",
        span_end});
  return true;
}

}  // namespace mb
