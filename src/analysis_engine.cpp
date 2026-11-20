#include "relay/analysis_engine.hpp"

#include <algorithm>
#include <chrono>
#include <fstream>
#include <sstream>
#include <thread>

#include "relay/bounded_pipeline.hpp"
#include "relay/caption_parser.hpp"
#include "relay/checkpoint_store.hpp"
#include "relay/sha256.hpp"
#include "relay/stream_probe.hpp"
#include "relay/transport_reorder.hpp"

namespace relay {
namespace {

constexpr size_t kChunkRecords = 256;
constexpr size_t kQueueChunks = 4;

std::string hash_file(Sha256& h, const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) return {};
  char buf[65536];
  while (in) {
    in.read(buf, sizeof(buf));
    h.update(buf, static_cast<size_t>(in.gcount()));
  }
  return "ok";
}

bool parse_packet_line(const std::string& line, PacketRecord& p) {
  if (line.empty() || line[0] == '#') return false;
  std::istringstream is(line);
  std::string tok;
  std::vector<int64_t> v;
  while (std::getline(is, tok, ',')) {
    try {
      v.push_back(std::stoll(tok));
    } catch (...) {
      return false;
    }
  }
  if (v.size() != 6) return false;
  p.node_id = static_cast<uint32_t>(v[0]);
  p.stream_id = static_cast<uint32_t>(v[1]);
  p.seq = static_cast<uint64_t>(v[2]);
  p.ts_ms = v[3];
  p.arrival_ms = v[4];
  p.payload_bytes = static_cast<uint32_t>(v[5]);
  return true;
}

int severity_rank(ErrorSeverity s) {
  switch (s) {
    case ErrorSeverity::Unrecoverable: return 2;
    case ErrorSeverity::UnknownOrigin: return 1;
    case ErrorSeverity::Recoverable: return 0;
  }
  return 0;
}

}  // namespace

std::vector<DamageRange> merge_damage_ranges(std::vector<DamageRange> in,
                                             int64_t tolerance_ms) {
  std::sort(in.begin(), in.end(), [](const DamageRange& a, const DamageRange& b) {
    if (a.kind != b.kind) return a.kind < b.kind;
    return rational_cmp(a.range.start_ms, b.range.start_ms) < 0;
  });
  std::vector<DamageRange> out;
  Rational tol{tolerance_ms, 1};
  for (auto& r : in) {
    if (!out.empty() && out.back().kind == r.kind) {
      Rational limit = rational_add(out.back().range.end_ms, tol);
      if (rational_cmp(r.range.start_ms, limit) <= 0) {
        if (rational_cmp(r.range.end_ms, out.back().range.end_ms) > 0)
          out.back().range.end_ms = r.range.end_ms;
        if (severity_rank(r.severity) > severity_rank(out.back().severity))
          out.back().severity = r.severity;
        continue;
      }
    }
    out.push_back(r);
  }
  std::sort(out.begin(), out.end(), [](const DamageRange& a, const DamageRange& b) {
    return rational_cmp(a.range.start_ms, b.range.start_ms) < 0;
  });
  return out;
}

EngineResult run_analysis(const EngineInputs& in, const EngineConfig& cfg) {
  EngineResult res;
  auto t0 = std::chrono::steady_clock::now();

  // 1. Bind the input: every input file feeds one running hash.
  Sha256 ih;
  for (const auto& p : {in.packets_path, in.media_path, in.captions_path}) {
    if (p.empty()) continue;
    if (hash_file(ih, p).empty()) {
      res.status = RunStatus::InputError;
      res.detail = "cannot_read_input:" + p;
      return res;
    }
  }
  const std::string input_hash = Sha256::hex(ih.final());

  // 2. Resume state.
  uint64_t cursor = 0;
  std::vector<DamageRange> ranges;
  uint64_t dup = 0, reord = 0, lost = 0, burst = 0;
  if (cfg.resume) {
    auto lr = load_checkpoint(cfg.checkpoint_path, input_hash, cfg.analysis_version);
    if (lr.status == LoadStatus::InputMismatch ||
        lr.status == LoadStatus::VersionMismatch) {
      res.status = RunStatus::CheckpointMismatch;
      res.detail = lr.detail;
      return res;
    }
    if (lr.status == LoadStatus::Corrupt) {
      res.status = RunStatus::CheckpointCorrupt;
      res.detail = lr.detail;
      return res;
    }
    if (lr.status == LoadStatus::Ok) {
      cursor = lr.checkpoint.cursor;
      dup = lr.checkpoint.duplicate_packets;
      reord = lr.checkpoint.reordered_packets;
      lost = lr.checkpoint.lost_packets;
      burst = lr.checkpoint.burst_losses;
      ranges = std::move(lr.checkpoint.completed_ranges);
    }
  }

  // 3. Captions (fast stage; redone deterministically on resume).
  if (!in.captions_path.empty()) {
    std::ifstream cs(in.captions_path, std::ios::binary);
    std::ostringstream ss;
    ss << cs.rdbuf();
    auto parsed = parse_captions(ss.str());
    for (const auto& a : parsed.anomalies) {
      if (!a.range) continue;
      DamageRange d;
      d.range = *a.range;
      d.kind = DamageKind::CaptionAnomaly;
      d.severity = a.severity;
      d.note = a.message;
      ranges.push_back(std::move(d));
    }
  }

  // 4. Media probe (deterministic; redone on resume).
  if (!in.media_path.empty()) {
    ProbeOptions po;
    auto probe = probe_media(in.media_path, po);
    if (!probe.ok) {
      res.status = RunStatus::InputError;
      res.detail = "probe_failed:" + probe.error;
      return res;
    }
    ranges.insert(ranges.end(), probe.damages.begin(), probe.damages.end());
  }

  // 5. Transport stage through the bounded pipeline.
  MemoryTracker mem(cfg.memory_limit_bytes);
  CancelToken cancel;
  BoundedQueue<std::vector<PacketRecord>> queue(kQueueChunks, mem);
  TransportReorderer reorder(cfg.packet_window);
  BandwidthEstimator bw;
  std::ifstream pin(in.packets_path);
  if (!pin) {
    res.status = RunStatus::InputError;
    res.detail = "cannot_read_input:" + in.packets_path;
    return res;
  }

  std::atomic<bool> producer_failed{false};
  std::thread producer([&] {
    std::string line;
    uint64_t index = 0;
    std::vector<PacketRecord> chunk;
    int64_t chunk_bytes = 0;
    auto flush_chunk = [&] {
      if (chunk.empty()) return;
      if (!queue.push(std::move(chunk), chunk_bytes, cancel))
          producer_failed = producer_failed || !cancel.cancelled();
      chunk.clear();
      chunk_bytes = 0;
    };
    while (std::getline(pin, line)) {
      PacketRecord p;
      if (!parse_packet_line(line, p)) continue;
      if (index++ < cursor) continue;  // already processed before interrupt
      chunk.push_back(p);
      chunk_bytes += p.payload_bytes;
      if (chunk.size() >= kChunkRecords) flush_chunk();
      if (cancel.cancelled()) break;
    }
    flush_chunk();
    queue.close();
  });

  uint64_t processed = cursor;
  uint64_t chunks_this_run = 0;
  bool interrupted = false;
  std::vector<PacketRecord> emitted;
  while (auto chunk = queue.pop(cancel)) {
    int64_t first_arrival = chunk->front().arrival_ms;
    int64_t last_arrival = chunk->back().arrival_ms;
    int64_t bytes = 0;
    for (const auto& p : *chunk) {
      reorder.push(p, emitted);
      bytes += p.payload_bytes;
    }
    emitted.clear();
    bw.update(bytes, last_arrival - first_arrival + 1);
    processed += chunk->size();
    ++chunks_this_run;
    if (cfg.interrupt_after_chunks > 0 &&
        chunks_this_run >= cfg.interrupt_after_chunks) {
      // Simulated interruption: cancel stages, release resources, persist
      // the resumable cursor plus everything completed so far.
      cancel.cancel();
      interrupted = true;
      break;
    }
  }
  if (!interrupted) {
    reorder.flush(emitted);
    emitted.clear();
  }
  producer.join();
  if (producer_failed && !interrupted) {
    res.status = RunStatus::InputError;
    res.detail = "pipeline_push_failed";
    return res;
  }

  // 6. Attribute transport gaps onto the unified axis.
  const auto& st = reorder.stats();
  dup += st.duplicate_packets;
  reord += st.reordered_packets;
  lost += st.lost_packets;
  burst += st.burst_losses;
  for (const auto& g : reorder.gaps()) {
    DamageRange d;
    d.range = {{g.start_ms, 1}, {g.end_ms, 1}};
    d.kind = DamageKind::TransportLoss;
    d.severity = ErrorSeverity::Recoverable;
    d.note = g.burst ? "burst_loss" : "packet_gap";
    ranges.push_back(std::move(d));
  }

  res.duplicate_packets = dup;
  res.reordered_packets = reord;
  res.lost_packets = lost;
  res.burst_losses = burst;
  res.peak_memory_bytes = mem.peak();
  res.est_bandwidth_kbps = bw.kbps();

  if (interrupted) {
    Checkpoint ck;
    ck.input_sha256 = input_hash;
    ck.analysis_version = cfg.analysis_version;
    ck.cursor = processed;
    ck.duplicate_packets = dup;
    ck.reordered_packets = reord;
    ck.lost_packets = lost;
    ck.burst_losses = burst;
    ck.completed_ranges = ranges;
    if (!save_checkpoint(cfg.checkpoint_path, ck)) {
      res.status = RunStatus::InputError;
      res.detail = "checkpoint_save_failed";
      return res;
    }
    res.status = RunStatus::Interrupted;
    res.detail = "checkpoint_saved";
    return res;
  }

  // 7. Merge, score against injected truth, checksum.
  res.damaged_ranges = merge_damage_ranges(std::move(ranges), 40);

  if (!in.expected_damage_path.empty()) {
    std::ifstream ex(in.expected_damage_path);
    std::string line;
    int64_t worst = 0;
    while (std::getline(ex, line)) {
      if (line.empty() || line[0] == '#') continue;
      std::istringstream ls(line);
      int64_t es = 0, ee = 0;
      std::string kind;
      if (!(ls >> es >> ee >> kind)) continue;
      int64_t best = INT64_MAX;
      for (const auto& d : res.damaged_ranges) {
        int64_t ds = rational_floor(d.range.start_ms);
        int64_t de = rational_floor(d.range.end_ms);
        if (de < es - 200 || ds > ee + 200) continue;  // must overlap-ish
        int64_t err = std::llabs(ds - es);
        best = std::min(best, err);
      }
      if (best == INT64_MAX) best = 1000000;  // missed damage entirely
      worst = std::max(worst, best);
    }
    res.boundary_error_ms = worst;
  }

  std::ostringstream canon;
  canon << "relay-result-v1\n";
  canon << "counts " << dup << " " << reord << " " << lost << " " << burst << "\n";
  for (const auto& d : res.damaged_ranges) {
    canon << "range " << rational_floor(d.range.start_ms) << " "
          << rational_floor(d.range.end_ms) << " " << to_string(d.kind) << " "
          << to_string(d.severity) << " " << d.note << "\n";
  }
  res.output_sha256 = Sha256::hex_of(canon.str());

  if (cfg.resume) {
    res.resume_ms = std::chrono::duration<double, std::milli>(
                        std::chrono::steady_clock::now() - t0)
                        .count();
  }
  res.status = RunStatus::Ok;
  return res;
}

}  // namespace relay
