#include "report.hpp"

#include <algorithm>
#include <cmath>

#include "timebase.hpp"

namespace mb {

int64_t max_subtitle_alignment_ms(const SubtitleDoc& doc,
                                  const Ratio& video_fps) {
  int64_t worst = 0;
  const int64_t frame_tick = kMasterHz * video_fps.den / video_fps.num;
  if (frame_tick <= 0) return 0;
  for (const SubCue& c : doc.cues) {
    // 对齐到最近帧采样点，偏差以主刻度整数计算后转毫秒（无浮点累积）。
    int64_t nearest = ((c.start_ticks + frame_tick / 2) / frame_tick)
                      * frame_tick;
    int64_t diff = std::llabs(c.start_ticks - nearest);
    worst = std::max(worst, master_to_ms(diff));
  }
  return worst;
}

ReportResult build_report(const ReportInputs& in, const CpError* cp_failure) {
  ReportResult rr;
  for (const ErrRecord& e : in.errors) rr.errors_by_code[e.code]++;

  // 指标判定。
  double dev = in.target_bitrate_bps > 0
      ? 100.0 * std::fabs(in.actual_bitrate_bps -
                          double(in.target_bitrate_bps)) /
            double(in.target_bitrate_bps)
      : 0.0;
  bool bitrate_ok = dev <= in.limits.max_bitrate_deviation_pct;
  bool align_ok =
      in.subtitle_alignment_max_ms <= in.limits.max_subtitle_alignment_ms;
  bool psnr_ok = in.video_psnr_db >= in.limits.min_video_psnr_db;
  bool queue_ok = in.queue_peak_items <= in.queue_capacity;
  rr.metrics_ok = bitrate_ok && align_ok && psnr_ok && queue_ok;
  rr.has_recoverable_errors = !in.errors.empty();

  JsonWriter w;
  w.key("target_bitrate_bps");
  w.val(in.target_bitrate_bps);
  w.key("actual_bitrate_bps");
  w.val(in.actual_bitrate_bps);
  w.key("bitrate_deviation_pct");
  w.val(dev);
  w.key("video_psnr_db");
  w.val(in.video_psnr_db);
  w.key("audio_peak_dbfs");
  w.val(in.audio_peak_dbfs);
  w.key("subtitle_alignment_max_ms");
  w.val(in.subtitle_alignment_max_ms);
  w.key("frames_exported");
  w.val(in.frames_exported);
  w.key("frame_sha256_count");
  w.val(in.frame_sha256_count);
  w.key("queue_capacity");
  w.val(static_cast<int64_t>(in.queue_capacity));
  w.key("queue_peak_items");
  w.val(static_cast<int64_t>(in.queue_peak_items));
  w.key("queue_peak_bytes");
  w.val(static_cast<int64_t>(in.queue_peak_bytes));
  w.key("queue_push_waits");
  w.val(static_cast<int64_t>(in.queue_push_waits));
  w.key("peak_memory_bytes");
  w.val(static_cast<int64_t>(in.peak_memory_bytes));
  w.key("checkpoint_resumed_blocks");
  w.val(static_cast<int64_t>(in.checkpoint_resumed_blocks));
  w.key("checkpoint_failed_blocks");
  w.val(static_cast<int64_t>(in.checkpoint_failed_blocks));
  w.key("metrics_ok");
  w.val(rr.metrics_ok);
  w.key("first_error_source_offset");
  w.val(in.first_error_offset);
  w.key("first_error_location");
  w.val(in.first_error_location);

  w.begin_arr("errors_by_code");
  for (const auto& [code, n] : rr.errors_by_code) {
    w.begin_obj();
    w.key("code");
    w.val(code);
    w.key("count");
    w.val(static_cast<int64_t>(n));
    w.end();
  }
  w.end();

  if (!in.extra.empty()) {
    w.begin_obj("extra");
    for (const auto& [k, v] : in.extra) {
      w.key(k);
      w.val(static_cast<int64_t>(v));
    }
    w.end();
  }

  if (cp_failure && static_cast<bool>(*cp_failure)) {
    w.begin_obj("checkpoint_error");
    w.key("code");
    w.val(cp_failure->code);
    w.key("message");
    w.val(cp_failure->message);
    w.key("location");
    w.val(cp_failure->location);
    w.end();
  }
  rr.json = w.str();
  return rr;
}

int decide_exit_code(const ReportResult& rr, const CpError* cp_failure,
                     bool arg_error) {
  if (arg_error) return 5;
  if (cp_failure && static_cast<bool>(*cp_failure)) return 3;
  if (!rr.metrics_ok) return 4;
  if (rr.has_recoverable_errors) return 2;
  return 0;
}

}  // namespace mb
