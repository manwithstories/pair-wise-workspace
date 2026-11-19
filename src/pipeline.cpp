#include "pipeline.hpp"

#include <atomic>
#include <fstream>
#include <sstream>
#include <thread>

extern "C" {
#include <libavutil/log.h>
}

#include "media.hpp"
#include "render.hpp"
#include "stream.hpp"
#include "subtitle.hpp"
#include "timeline.hpp"

namespace mb {

namespace fs = std::filesystem;

namespace {
// 读入整个文件为字节串（夹具由本程序生成，路径已受工作目录约束）。
bool read_bytes(const fs::path& p, std::string* out) {
  std::ifstream f(p, std::ios::binary);
  if (!f) return false;
  std::ostringstream ss;
  ss << f.rdbuf();
  *out = ss.str();
  return true;
}
}  // namespace

PipelineOutcome run_pipeline(const PipelineConfig& cfg) {
  av_log_set_level(AV_LOG_QUIET);  // 完全静默；解码错误经返回码进入报告
  PipelineOutcome po;
  ReportInputs& ri = po.report;
  ri.target_bitrate_bps = cfg.target_bitrate_bps;
  ri.queue_capacity = cfg.queue_capacity;
  ErrList errs;

  // ---- 1. 夹具（已生成则复用；由 main 决定是否重新生成）----
  FixtureConfig fcfg{cfg.work_dir, cfg.damaged,
                     1'500'000, Ratio{25, 1}, 2000};
  FixtureSet fx;
  std::string fgen_fatal;
  if (!generate_fixtures(fcfg, &fx, &errs, &fgen_fatal)) {
    po.fatal = fgen_fatal;
    po.exit_code = 5;
    return po;
  }

  // ---- 2. 字幕解析（错误继续，保留源偏移）----
  std::string srt_bytes;
  read_bytes(fs::path(fx.srt_path), &srt_bytes);
  SubtitleDoc srt = parse_subtitles(srt_bytes, SubFormat::kSrt);
  for (const auto& e : srt.errors) errs.push_back(e);
  std::string vtt_bytes;
  read_bytes(fs::path(fx.vtt_path), &vtt_bytes);
  SubtitleDoc vtt = parse_subtitles(vtt_bytes, SubFormat::kVtt);
  for (const auto& e : vtt.errors) errs.push_back(e);
  ri.subtitle_alignment_max_ms =
      max_subtitle_alignment_ms(srt, fx.media_spec.fps);

  // ---- 3. 时间线：节目轨（高优先级）+ 字幕轨 ----
  std::vector<TrackSpec> tracks;
  const int64_t frame_tick =
      kMasterHz * fx.media_spec.fps.den / fx.media_spec.fps.num;
  const int64_t total_ticks =
      fx.media_spec.duration_ms * kMasterHz / 1000;
  TrackSpec program;
  program.id = "program";
  program.role = "program";
  program.priority = 10;
  program.boundary_end = total_ticks;
  program.clips.push_back(
      {"program.mkv", 0, total_ticks, 0});
  tracks.push_back(program);
  TrackSpec subtrack;
  subtrack.id = "subtitle";
  subtrack.role = "subtitle";
  subtrack.priority = 5;
  subtrack.boundary_end = total_ticks;
  for (const SubCue& c : srt.cues)
    subtrack.clips.push_back({"srt", c.start_ticks, c.end_ticks,
                              c.source_offset});
  if (!subtrack.clips.empty()) tracks.push_back(subtrack);
  Timeline tl = compose_timeline(tracks, total_ticks);
  for (const auto& d : tl.decisions)
    errs.push_back({d.code, d.message, d.source_offset});

  // ---- 4. 检查点：打开/校验，决定续跑块集合 ----
  std::string digest = fixture_manifest_digest(fx);
  CheckpointConfig cpcfg{cfg.work_dir, digest, cfg.damaged ? "damaged" : "good"};
  CheckpointStore cp;
  if (!cp.open(cpcfg, &po.checkpoint_error)) {
    po.exit_code = 3;
    ri.errors = errs;
    return po;
  }
  ResumeResult resume;
  if (cfg.resume) {
    if (!cp.resume(&resume, &po.checkpoint_error)) {
      po.exit_code = 3;
      ri.errors = errs;
      return po;
    }
  }
  ri.checkpoint_resumed_blocks = resume.resumed_blocks;
  ri.checkpoint_failed_blocks = resume.failed_blocks.size();

  // ---- 5. 有界队列流：帧级生产 -> 慢消费者攒块写检查点（背压）----
  const int64_t total_frames =
      fx.media_spec.duration_ms * fx.media_spec.fps.num /
      (1000 * fx.media_spec.fps.den);
  int block_count = static_cast<int>(
      (total_frames + cfg.chunk_frames - 1) / cfg.chunk_frames);
  std::vector<bool> block_done(block_count, false);
  for (int id : resume.complete_blocks)
    if (id >= 0 && id < block_count) block_done[id] = true;

  // 队列承载帧级工作项：每项 64 字节确定性负载，队列满即阻塞（背压）。
  BoundedQueue q(cfg.queue_capacity, cfg.slow_consumer_ms);
  std::atomic<bool> producer_ok{true};
  std::atomic<int64_t> produced{0};
  // 仍需处理的帧集合（跳过属于已完成检查点块的帧）。
  std::vector<int64_t> pending_frames;
  for (int64_t f = 0; f < total_frames; ++f) {
    int blk = static_cast<int>(f / cfg.chunk_frames);
    if (!block_done[blk]) pending_frames.push_back(f);
  }

  std::thread producer([&] {
    for (int64_t f : pending_frames) {
      std::vector<uint8_t> item(64);
      for (int j = 0; j < 64; ++j)
        item[j] = static_cast<uint8_t>((f * 17 + j) & 0xFF);
      if (!q.push(std::move(item))) { producer_ok = false; return; }
      ++produced;
    }
    q.close();
  });

  // 消费者：按 chunk_frames 攒帧，凑满一个块即原子写检查点并更新索引。
  CpError consumer_err;
  std::vector<uint8_t> block_buf;
  int64_t block_first_frame = -1;
  int64_t block_frame_count = 0;
  int64_t consumed_so_far = 0;  // 已消费的 pending 帧计数
  auto flush_block = [&](int64_t first, int64_t count) -> bool {
    if (count <= 0) return true;
    int id = static_cast<int>(first / cfg.chunk_frames);
    int64_t s = first * frame_tick;
    int64_t e = (first + count) * frame_tick;
    if (!cp.write_block(id, s, e, count, block_buf, &consumer_err))
      return false;
    block_done[id] = true;
    block_buf.clear();
    return true;
  };
  while (auto item = q.pop()) {
    int64_t frame_no =
        consumed_so_far < static_cast<int64_t>(pending_frames.size())
            ? pending_frames[consumed_so_far]
            : consumed_so_far;
    if (block_frame_count == 0) block_first_frame = frame_no;
    block_buf.insert(block_buf.end(), item->begin(), item->end());
    ++block_frame_count;
    ++consumed_so_far;
    if (block_frame_count >= cfg.chunk_frames) {
      if (!flush_block(block_first_frame, block_frame_count)) {
        producer_ok = false;
        break;
      }
      block_first_frame = -1;
      block_frame_count = 0;
    }
  }
  // 写最后一个不足整帧块的尾块（明确处理末段）。
  if (producer_ok && block_frame_count > 0 &&
      !flush_block(block_first_frame, block_frame_count)) {
    producer_ok = false;
  }
  producer.join();
  ri.queue_peak_items = q.peak_items();
  ri.queue_peak_bytes = q.peak_bytes();
  ri.queue_push_waits = q.push_wait_count();
  ri.peak_memory_bytes = q.peak_bytes();  // 峰值驻留即内存估算上界
  if (static_cast<bool>(consumer_err)) {
    po.checkpoint_error = consumer_err;
    ri.errors = errs;
    po.exit_code = 3;
    return po;
  }

  // ---- 6. FFmpeg 再封装（目标码率/封装），失败区间标记检查点 ----
  std::string out_media = (cfg.work_dir / "remux.mkv").string();
  TranscodeResult tr;
  ErrList terrs;
  bool tok = transcode_media(fx.media_path, out_media, fx.media_spec,
                             cfg.target_bitrate_bps, cfg.container, &tr,
                             &terrs);
  for (const auto& e : terrs) errs.push_back(e);
  if (!tok) {
    cp.mark_block_failed(block_count, total_ticks, total_ticks + frame_tick,
                         nullptr);
    ri.checkpoint_failed_blocks++;
  }
  for (auto [a, b] : tr.failed_ranges)
    cp.mark_block_failed(block_count, a, b, nullptr);
  ri.actual_bitrate_bps = tr.stats.actual_video_bitrate_bps;

  // ---- 7. 媒体探测：PSNR / 音频峰值 / 解码错误（容忍继续）----
  MediaStats st;
  ErrList perrs;
  probe_media(fx.media_path, fx.media_spec, &st, true, &perrs);
  for (const auto& e : perrs) errs.push_back(e);
  ri.video_psnr_db = st.video_psnr_db;
  ri.audio_peak_dbfs = st.audio_peak_dbfs;
  if (st.decode_errors > 0)
    ri.extra["damaged_records"] = st.decode_errors;
  if (!errs.empty()) {
    ri.first_error_offset = errs.front().source_offset;
    ri.first_error_location = "offset:" +
        std::to_string(errs.front().source_offset);
  }

  // ---- 8. 离屏 PNG 帧导出与帧清单 ----
  RenderConfig rc;
  rc.out_dir = cfg.work_dir / "frames";
  rc.media = fx.media_spec;
  rc.timeline = &tl;
  rc.emit_frames = cfg.emit_frames;
  RenderResult rr;
  ErrList rerrs;
  render_frames(rc, &rr, &rerrs);
  for (const auto& e : rerrs) errs.push_back(e);
  ri.frames_exported = rr.exported;
  ri.frame_sha256_count = static_cast<int64_t>(rr.frames.size());

  cp.finalize(nullptr);
  ri.errors = errs;
  ReportResult built = build_report(ri, nullptr);
  po.exit_code = decide_exit_code(built, nullptr, false);
  return po;
}

}  // namespace mb
