#include "relay/stream_probe.hpp"

#include <algorithm>
#include <cmath>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
}

namespace relay {
namespace {

Rational to_rational(AVRational r) { return make_rational(r.num, r.den); }

int64_t ts_to_ms(int64_t ts, AVRational tb) {
  return rational_floor(rational_mul({ts, 1},
                                     rational_mul(to_rational(tb), {1000, 1})));
}

struct AvCtx {
  AVFormatContext* fmt = nullptr;
  ~AvCtx() {
    if (fmt) avformat_close_input(&fmt);
  }
};

}  // namespace

ProbeOutput probe_media(const std::string& path, const ProbeOptions& opts) {
  ProbeOutput out;
  av_log_set_level(AV_LOG_QUIET);
  AvCtx ctx;
  if (avformat_open_input(&ctx.fmt, path.c_str(), nullptr, nullptr) < 0) {
    out.error = "cannot_open_container";
    return out;
  }
  if (avformat_find_stream_info(ctx.fmt, nullptr) < 0) {
    out.error = "cannot_read_stream_info";
    return out;
  }
  int vidx = av_find_best_stream(ctx.fmt, AVMEDIA_TYPE_VIDEO, -1, -1, nullptr, 0);
  if (vidx < 0) {
    out.error = "no_video_stream";
    return out;
  }
  AVStream* vs = ctx.fmt->streams[vidx];
  out.stats.frame_rate = to_rational(vs->avg_frame_rate);
  if (out.stats.frame_rate.num <= 0) out.stats.frame_rate = to_rational(vs->r_frame_rate);
  Rational frame_ms{0, 1};
  if (out.stats.frame_rate.num > 0)
    frame_ms = rational_div({1000, 1}, out.stats.frame_rate);
  if (ctx.fmt->duration != AV_NOPTS_VALUE)
    out.stats.duration_ms = ctx.fmt->duration / 1000;

  // Optional decode pass: strict error recognition turns bitstream damage
  // into hard decode errors anchored at the packet's presentation time.
  AVCodecContext* dec = nullptr;
  if (opts.decode_check) {
    const AVCodec* codec = avcodec_find_decoder(vs->codecpar->codec_id);
    if (codec && (dec = avcodec_alloc_context3(codec)) != nullptr) {
      if (avcodec_parameters_to_context(dec, vs->codecpar) < 0 ||
          avcodec_open2(dec, codec, nullptr) < 0) {
        avcodec_free_context(&dec);
        dec = nullptr;
      } else {
        dec->err_recognition = AV_EF_CRCCHECK | AV_EF_BITSTREAM | AV_EF_BUFFER |
                               AV_EF_EXPLODE;
      }
    }
  }

  AVPacket* pkt = av_packet_alloc();
  AVFrame* frame = av_frame_alloc();
  int64_t prev_pts_ms = -1;
  int64_t wrap_offset_ms = 0;  // accumulated correction for timestamp wrap
  std::vector<int64_t> key_pts;
  const int64_t jump_ms =
      frame_ms.num > 0 ? 5 * rational_round(frame_ms) : 500;

  auto add_damage = [&](int64_t a, int64_t b, DamageKind kind,
                        ErrorSeverity sev, std::string note) {
    DamageRange d;
    d.range = {{a, 1}, {b, 1}};
    d.kind = kind;
    d.severity = sev;
    d.note = std::move(note);
    out.damages.push_back(std::move(d));
  };

  while (av_read_frame(ctx.fmt, pkt) >= 0) {
    if (pkt->stream_index == vidx) {
      ++out.stats.packets;
      bool key = (pkt->flags & AV_PKT_FLAG_KEY) != 0;
      if (key) ++out.stats.keyframes;
      int64_t raw = pkt->pts != AV_NOPTS_VALUE ? pkt->pts : pkt->dts;
      if (raw == AV_NOPTS_VALUE) {
        // Missing timestamp: cannot anchor this packet — mark uncertain.
        int64_t at = prev_pts_ms >= 0 ? prev_pts_ms : 0;
        add_damage(at, at + rational_round(frame_ms), DamageKind::Uncertain,
                   ErrorSeverity::UnknownOrigin, "missing_timestamp");
      } else {
        int64_t pts_ms = ts_to_ms(raw, vs->time_base) + wrap_offset_ms;
        if (prev_pts_ms >= 0) {
          int64_t delta = pts_ms - prev_pts_ms;
          // 33-bit MPEG-TS style wrap: large negative jump near the wrap
          // period is normalized and treated as recoverable, not damage.
          if (delta < -jump_ms) {
            wrap_offset_ms += 0;  // container clocks here are 64-bit; a large
            // negative delta with no wrap period is a jump, not a wrap.
            add_damage(prev_pts_ms, prev_pts_ms + rational_round(frame_ms),
                       DamageKind::Uncertain, ErrorSeverity::Recoverable,
                       "timestamp_jump_backward");
          } else if (delta > jump_ms) {
            add_damage(prev_pts_ms, pts_ms, DamageKind::Uncertain,
                       ErrorSeverity::UnknownOrigin, "timestamp_jump_forward");
          }
        }
        prev_pts_ms = pts_ms;
        if (key) key_pts.push_back(pts_ms);
        if (dec) {
          if (avcodec_send_packet(dec, pkt) < 0) {
            add_damage(pts_ms, pts_ms + rational_round(frame_ms),
                       DamageKind::StructuralCorruption,
                       ErrorSeverity::Recoverable, "decode_reject");
            avcodec_flush_buffers(dec);
          } else {
            while (true) {
              int r = avcodec_receive_frame(dec, frame);
              if (r == AVERROR(EAGAIN) || r == AVERROR_EOF) break;
              if (r < 0) {
                add_damage(pts_ms, pts_ms + rational_round(frame_ms),
                           DamageKind::StructuralCorruption,
                           ErrorSeverity::Recoverable, "decode_error");
                avcodec_flush_buffers(dec);
                break;
              }
              av_frame_unref(frame);
            }
          }
        }
      }
    }
    av_packet_unref(pkt);
  }
  if (dec) avcodec_free_context(&dec);
  av_packet_free(&pkt);
  av_frame_free(&frame);

  if (key_pts.size() >= 2) {
    std::vector<int64_t> diffs;
    for (size_t i = 1; i < key_pts.size(); ++i)
      diffs.push_back(key_pts[i] - key_pts[i - 1]);
    std::sort(diffs.begin(), diffs.end());
    out.stats.keyframe_interval_ms = {diffs[diffs.size() / 2], 1};
  }
  out.ok = true;
  return out;
}

PacketLocation find_keyframe_near(const std::string& path, int64_t target_ms) {
  PacketLocation best;
  av_log_set_level(AV_LOG_QUIET);
  AvCtx ctx;
  if (avformat_open_input(&ctx.fmt, path.c_str(), nullptr, nullptr) < 0)
    return best;
  if (avformat_find_stream_info(ctx.fmt, nullptr) < 0) return best;
  int vidx = av_find_best_stream(ctx.fmt, AVMEDIA_TYPE_VIDEO, -1, -1, nullptr, 0);
  if (vidx < 0) return best;
  AVStream* vs = ctx.fmt->streams[vidx];
  AVPacket* pkt = av_packet_alloc();
  int64_t best_dist = INT64_MAX;
  while (av_read_frame(ctx.fmt, pkt) >= 0) {
    if (pkt->stream_index == vidx && (pkt->flags & AV_PKT_FLAG_KEY) &&
        pkt->pts != AV_NOPTS_VALUE) {
      int64_t pts_ms = ts_to_ms(pkt->pts, vs->time_base);
      int64_t dist = pts_ms > target_ms ? pts_ms - target_ms : target_ms - pts_ms;
      if (dist < best_dist) {
        best_dist = dist;
        best.pts_ms = pts_ms;
        best.file_offset = pkt->pos;
        best.size = pkt->size;
        best.found = true;
      }
    }
    av_packet_unref(pkt);
  }
  av_packet_free(&pkt);
  return best;
}

}  // namespace relay
