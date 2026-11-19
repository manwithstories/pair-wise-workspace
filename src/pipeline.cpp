#include "media/pipeline.hpp"

#include <algorithm>
#include <cmath>
#include <cstdio>

#include "media/av_sync.hpp"
#include "media/checkpoint_index.hpp"
#include "media/jitter_buffer.hpp"
#include "media/recovery.hpp"
#include "media/sample_generator.hpp"
#include "media/stream_reader.hpp"

namespace media {

namespace {

std::vector<Packet> build_video_packets(const SampleBundle& b) {
  std::vector<Packet> out;
  out.reserve(b.frames.size());
  for (const auto& f : b.frames) {
    Packet p;
    p.kind = MediaKind::Video;
    p.codec = f.codec;
    p.pts_us = f.pts_us;
    p.dts_us = f.dts_us;
    p.frame_index = f.index;
    p.keyframe = f.keyframe;
    p.payload = f.data;
    out.push_back(std::move(p));
  }
  return out;
}

double percentile95(std::vector<std::int64_t> v) {
  if (v.empty()) return 0.0;
  std::sort(v.begin(), v.end());
  const std::size_t i = std::min(v.size() - 1, (v.size() * 95) / 100);
  return static_cast<double>(v[i]);
}

void tally_errors(PipelineMetrics& m, const std::vector<ErrorEvent>& errs) {
  for (const auto& e : errs)
    ++m.errors_by_code[error_code_name(e.code)];
}

// Persist emitted items as fixed-size checkpoint blocks, resuming after the
// valid durable prefix. Returns 3 when the checkpoint cannot be recovered.
int checkpoint_stage(const PipelineConfig& cfg, const SampleBundle& bundle,
                     PipelineMetrics& m,
                     const std::vector<SyncItem>& video_items) {
  if (cfg.checkpoint_dir.empty()) return 0;
  CheckpointIndex cp(cfg.checkpoint_dir);
  CheckpointStatus status;
  CheckpointState prior = cp.load(status);
  if (status == CheckpointStatus::Unrecoverable) {
    m.checkpoint_corrupt = true;
    ++m.errors_by_code[error_code_name(ErrorCode::CheckpointUnrecoverable)];
    return 3;
  }
  if (status == CheckpointStatus::RolledBack)
    ++m.errors_by_code[error_code_name(ErrorCode::CheckpointCorrupt)];
  m.checkpoint_resumed = prior.blocks.size();

  constexpr std::uint32_t kBlockItems = 24;
  const std::uint32_t total = static_cast<std::uint32_t>(
      (video_items.size() + kBlockItems - 1) / kBlockItems);
  for (std::uint32_t block = 0; block < total; ++block) {
    if (block < prior.blocks.size()) continue; // already durable
    if (cfg.inject_interrupt > 0 && block == cfg.inject_interrupt) {
      CheckpointBlock partial;
      partial.block_id = block;
      partial.input_hash = bundle.input_hash;
      std::string err;
      cp.simulate_interrupt(partial, err);
      m.interrupted = true;
      return 0; // process stops; the resume command continues later
    }
    const std::size_t begin = static_cast<std::size_t>(block) * kBlockItems;
    const std::size_t end = std::min(video_items.size(), begin + kBlockItems);
    CheckpointBlock b;
    b.block_id = block;
    b.input_hash = bundle.input_hash;
    b.start_us = video_items[begin].clock_us;
    b.end_us = video_items[end - 1].clock_us;
    b.packet_count = end - begin;
    std::string err;
    if (!cp.write_block(b, err)) {
      m.checkpoint_corrupt = true;
      ++m.errors_by_code[error_code_name(ErrorCode::CheckpointUnrecoverable)];
      return 3;
    }
    ++m.blocks_written;
  }
  return 0;
}

} // namespace

int run_pipeline(const PipelineConfig& cfg, PipelineMetrics& m) {
  // ---- Stage 1: deterministic sample generation -----------------------------
  SampleConfig sc;
  sc.codec = cfg.codec;
  sc.seed = cfg.seed;
  sc.frame_count = cfg.frame_count;
  sc.truncated = cfg.truncated_stream;
  SampleBundle bundle = generate_sample(sc);

  StreamIndex supplied = bundle.index;
  if (cfg.use_faulty_index)
    supplied = make_missing_entry_index(bundle);
  const std::uint64_t expected_size =
      cfg.truncated_stream ? bundle.video.size() : 0;
  ReadResult read = read_stream(
      cfg.truncated_stream ? make_truncated_stream(bundle) : bundle.video,
      cfg.codec, &supplied, expected_size);
  tally_errors(m, read.errors);
  if (read.truncated && cfg.truncated_stream)
    ++m.errors_by_code[error_code_name(ErrorCode::StreamTruncated)];

  // ---- Stage 2: network replay -----------------------------------------------
  NetworkConfig nc;
  nc.seed = cfg.seed;
  nc.loss_burst = cfg.loss_burst;
  nc.bandwidth_kbps = cfg.bandwidth_kbps;
  ReplayOutput replayed = network_replay(build_video_packets(bundle), nc);
  m.packets_duplicate = replayed.stats.packets_duplicate;
  m.packets_reordered = replayed.stats.packets_reordered;
  m.packets_dropped = replayed.stats.packets_dropped;
  m.total_packets = replayed.stats.packets_in;

  // ---- Stage 3: jitter buffer + recovery -------------------------------------
  // Drive the playout clock from actual arrival timestamps: each packet is
  // drained once its own arrival is older than the adaptive target depth, so
  // natural spacing is preserved and only real losses become gaps.
  JitterBuffer jb;
  RecoveryEngine recovery;
  AVSync sync;
  std::vector<SyncItem> video_items;
  std::int64_t playout_now = 0;
  std::uint64_t playout_index = 0; // contiguous receive index, gap-free
  for (std::uint32_t i = 0; i < replayed.packets.size(); ++i) {
    if (!cfg.bandwidth_kbps.empty())
      jb.update_bandwidth(cfg.bandwidth_kbps[
          std::min(i / 60u,
                   static_cast<std::uint32_t>(cfg.bandwidth_kbps.size()) - 1)]);
    const std::int64_t arrival = replayed.stats.arrival_us[i];
    // Monotonic playout clock: never move backward even if a reordered packet
    // carries an earlier arrival timestamp.
    playout_now = std::max(playout_now, arrival);
    jb.push(replayed.packets[i], arrival);
    std::vector<Gap> gaps;
    auto emitted = jb.drain(playout_now, &gaps);
    for (const auto& g : gaps)
      recovery.handle_gap(g, replayed.packets);
    for (auto& p : emitted) {
      const std::int64_t clock =
          sync.push_video(playout_index++, p.pts_us, false);
      SyncItem it;
      it.kind = MediaKind::Video;
      it.source_index = p.frame_index;
      it.clock_us = clock;
      it.raw_pts_us = p.pts_us;
      video_items.push_back(std::move(it));
    }
  }
  std::vector<Gap> tail_gaps;
  for (auto& p : jb.flush(&tail_gaps)) {
    const std::int64_t clock =
        sync.push_video(playout_index++, p.pts_us, false);
    SyncItem it;
    it.kind = MediaKind::Video;
    it.source_index = p.frame_index;
    it.clock_us = clock;
    video_items.push_back(std::move(it));
  }
  for (const auto& g : tail_gaps)
    recovery.handle_gap(g, replayed.packets);

  // ---- Stage 4: audio on the same clock --------------------------------------
  std::vector<SyncItem> audio_items;
  for (const auto& blk : bundle.audio)
    audio_items.push_back(sync.push_audio(blk, false));
  sync.finalize();

  // ---- Stage 5: metrics -------------------------------------------------------
  m.recovery_rate = recovery_rate(recovery.stats());
  m.sync_error_ms_p95 = sync.stats().p95_error_us / 1000.0;
  m.latency_p95_ms = percentile95(jb.stats().latency_us) / 1000.0;
  for (const auto& ev : sync.stats().events)
    ++m.errors_by_code[error_code_name(ev.code)];
  for (const auto& rec : recovery.records())
    if (!rec.success)
      ++m.errors_by_code[error_code_name(ErrorCode::PacketUnrecoverable)];

  // ---- Stage 6: checkpointed block index -------------------------------------
  return checkpoint_stage(cfg, bundle, m, video_items);
}

std::string format_metrics(const PipelineMetrics& m) {
  std::string out;
  char line[256];
  auto emit = [&](const char* k, double v) {
    std::snprintf(line, sizeof(line), "%s=%.3f\n", k, v);
    out += line;
  };
  emit("sync_error_ms", m.sync_error_ms_p95);
  emit("recovery_rate", m.recovery_rate);
  emit("latency_p95_ms", m.latency_p95_ms);
  auto emit64 = [&](const char* k, std::uint64_t v) {
    std::snprintf(line, sizeof(line), "%s=%llu\n", k,
                  static_cast<unsigned long long>(v));
    out += line;
  };
  emit64("packets_duplicate", m.packets_duplicate);
  emit64("packets_reordered", m.packets_reordered);
  emit64("packets_dropped", m.packets_dropped);
  emit64("checkpoint_resumed", m.checkpoint_resumed);
  emit64("blocks_written", m.blocks_written);
  emit64("total_packets", m.total_packets);
  out += "errors_by_code=";
  if (m.errors_by_code.empty()) {
    out += "none";
  } else {
    bool first = true;
    for (const auto& [code, n] : m.errors_by_code) {
      if (!first) out += ",";
      std::snprintf(line, sizeof(line), "%s:%llu", code.c_str(),
                    static_cast<unsigned long long>(n));
      out += line;
      first = false;
    }
  }
  out += "\n";
  return out;
}

} // namespace media
