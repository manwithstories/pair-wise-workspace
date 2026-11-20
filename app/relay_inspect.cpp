// relay_inspect: terminal-only acceptance adapter.
// Subcommands: gen-packets, gen-captions, damage-media, analyze.
// Exit codes: 0 ok, 2 invalid args/input, 3 interrupted (checkpoint saved),
// 4 checkpoint mismatch/corrupt.

#include <cinttypes>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <random>
#include <string>
#include <unordered_map>
#include <vector>

#include "relay/analysis_engine.hpp"
#include "relay/stream_probe.hpp"

namespace {

using Args = std::unordered_map<std::string, std::string>;

Args parse_args(int argc, char** argv, int start) {
  Args a;
  for (int i = start; i < argc; ++i) {
    std::string k = argv[i];
    if (k.rfind("--", 0) == 0 && i + 1 < argc) {
      a[k.substr(2)] = argv[++i];
    } else if (k.rfind("--", 0) == 0) {
      a[k.substr(2)] = "1";  // bare flag
    }
  }
  return a;
}

std::string get(const Args& a, const char* k, const char* dflt = "") {
  auto it = a.find(k);
  return it == a.end() ? dflt : it->second;
}
int64_t get_i(const Args& a, const char* k, int64_t dflt = 0) {
  auto it = a.find(k);
  return it == a.end() ? dflt : std::stoll(it->second);
}

// ---- gen-packets ---------------------------------------------------------
// Deterministic transport recording: duplicates, reorder, burst loss and a
// continuously varying arrival (bandwidth) profile.
int cmd_gen_packets(const Args& a) {
  uint64_t seed = static_cast<uint64_t>(get_i(a, "seed", 1));
  int64_t count = get_i(a, "count", 1500);
  bool clean = a.count("clean") != 0;
  double dup_rate = std::stod(get(a, "dup-rate", "0.02"));
  double reorder_rate = std::stod(get(a, "reorder-rate", "0.05"));
  int64_t burst_at = get_i(a, "burst-loss-at-ms", -1);
  int64_t burst_len = get_i(a, "burst-loss-count", 0);
  std::string expected_out = get(a, "expected-out");

  std::mt19937_64 rng(seed);
  std::uniform_int_distribution<int64_t> payload(1100, 1500);
  std::uniform_real_distribution<double> uni(0.0, 1.0);

  std::vector<std::string> lines;
  lines.reserve(static_cast<size_t>(count) * 2);
  int64_t arrival = 0;
  int64_t held = -1;  // one-packet reorder stash
  std::string held_line;
  int64_t bw_phase = 0;
  for (int64_t seq = 0; seq < count; ++seq) {
    int64_t ts = seq * 40;
    // Varying bandwidth: arrival interval wanders between 10 and 120 ms.
    bw_phase += 1;
    int64_t step = 10 + (bw_phase % 97) * 110 / 97;
    arrival += step;
    if (!clean && burst_at >= 0 && ts >= burst_at &&
        ts < burst_at + burst_len * 40) {
      continue;  // burst-lost packet
    }
    char buf[128];
    std::snprintf(buf, sizeof(buf), "1,1,%" PRId64 ",%" PRId64 ",%" PRId64 ",%" PRId64,
                  seq, ts, arrival, payload(rng));
    std::string line = buf;
    if (!clean && uni(rng) < reorder_rate && held < 0) {
      held = seq;
      held_line = line;
      continue;  // emit after the next packet
    }
    if (held >= 0) {
      lines.push_back(held_line);
      held = -1;
    }
    lines.push_back(line);
    if (!clean && uni(rng) < dup_rate) lines.push_back(line);  // duplicate
  }
  if (held >= 0) lines.push_back(held_line);

  std::ofstream out(get(a, "out"));
  if (!out) return 2;
  out << "# node,stream,seq,ts_ms,arrival_ms,bytes\n";
  for (const auto& l : lines) out << l << "\n";
  if (!expected_out.empty() && burst_at >= 0 && burst_len > 0) {
    std::ofstream ex(expected_out);
    ex << burst_at - 40 << " " << burst_at + burst_len * 40
       << " transport_loss\n";
  }
  return 0;
}

// ---- gen-captions --------------------------------------------------------
int cmd_gen_captions(const Args& a) {
  std::string fmt = get(a, "format", "srt");
  int64_t count = get_i(a, "count", 8);
  bool anomalies = a.count("with-anomalies") != 0;
  std::ofstream out(get(a, "out"), std::ios::binary);
  if (!out) return 2;
  auto ts = [&](int64_t ms, char sep) {
    char b[32];
    std::snprintf(b, sizeof(b), "%02" PRId64 ":%02" PRId64 ":%02" PRId64 "%c%03" PRId64,
                  ms / 3600000, (ms / 60000) % 60, (ms / 1000) % 60, sep,
                  ms % 1000);
    return std::string(b);
  };
  char sep = fmt == "vtt" ? '.' : ',';
  if (fmt == "vtt") out << "WEBVTT\n\n";
  for (int64_t i = 0; i < count; ++i) {
    int64_t s = 500 + i * 1000;
    if (fmt == "srt") out << (i + 1) << "\n";
    out << ts(s, sep) << " --> " << ts(s + 800, sep) << "\ncue " << (i + 1)
        << "\n\n";
  }
  if (anomalies) {
    if (fmt == "srt") out << (count + 1) << "\n";
    out << "99:99:99" << sep << "999 --> 99:99:99" << sep << "999\n\n";  // invalid
    if (fmt == "srt") out << (count + 2) << "\n";
    out << ts(1500, sep) << " --> " << ts(2300, sep) << "\noverlap\n\n";
    if (fmt == "srt") out << (count + 3) << "\n";
    out << ts(9500, sep) << " --> " << ts(9800, sep) << "\ntruncated tail";
    // note: no trailing newline on the final cue
  }
  return 0;
}

// ---- damage-media --------------------------------------------------------
int cmd_damage_media(const Args& a) {
  std::string in = get(a, "in"), out = get(a, "out");
  int64_t at_ms = get_i(a, "at-ms", 3000);
  std::string expected_out = get(a, "expected-out");
  relay::ProbeOptions po;
  po.decode_check = false;
  auto probe = relay::probe_media(in, po);
  if (!probe.ok) {
    std::fprintf(stderr, "probe failed: %s\n", probe.error.c_str());
    return 2;
  }
  auto loc = relay::find_keyframe_near(in, at_ms);
  if (!loc.found || loc.file_offset < 0 || loc.size <= 0) {
    std::fprintf(stderr, "no keyframe near %" PRId64 " ms\n", at_ms);
    return 2;
  }
  std::ifstream src(in, std::ios::binary);
  std::ofstream dst(out, std::ios::binary);
  dst << src.rdbuf();
  dst.flush();
  if (!dst) return 2;
  std::fstream rw(out, std::ios::in | std::ios::out | std::ios::binary);
  rw.seekp(loc.file_offset);
  std::vector<char> zeros(static_cast<size_t>(loc.size), 0);
  rw.write(zeros.data(), loc.size);
  rw.flush();
  if (!rw) return 2;
  int64_t frame_ms = probe.stats.frame_rate.num > 0
                         ? relay::rational_round(
                               relay::rational_div({1000, 1}, probe.stats.frame_rate))
                         : 40;
  if (!expected_out.empty()) {
    std::ofstream ex(expected_out);
    ex << loc.pts_ms << " " << loc.pts_ms + frame_ms
       << " structural_corruption\n";
  }
  std::fprintf(stderr, "damaged keyframe at pts_ms=%" PRId64 " offset=%" PRId64
                 " size=%" PRId64 "\n", loc.pts_ms, loc.file_offset, loc.size);
  return 0;
}

// ---- analyze -------------------------------------------------------------
int cmd_analyze(const Args& a) {
  relay::EngineInputs in;
  in.packets_path = get(a, "packets");
  in.media_path = get(a, "media");
  in.captions_path = get(a, "captions");
  in.expected_damage_path = get(a, "expected-damage");
  relay::EngineConfig cfg;
  cfg.packet_window = static_cast<size_t>(get_i(a, "packet-window", 64));
  cfg.memory_limit_bytes = get_i(a, "memory-mib", 32) << 20;
  cfg.interrupt_after_chunks =
      static_cast<uint64_t>(get_i(a, "interrupt-after", 0));
  cfg.resume = a.count("resume") != 0;
  cfg.checkpoint_path = get(a, "checkpoint", "checkpoint.ckpt");
  if (in.packets_path.empty()) {
    std::fprintf(stderr, "analyze: --packets is required\n");
    return 2;
  }
  auto r = relay::run_analysis(in, cfg);
  if (r.status == relay::RunStatus::Interrupted) {
    std::printf("status=interrupted\n");
    return 3;
  }
  if (r.status == relay::RunStatus::CheckpointMismatch ||
      r.status == relay::RunStatus::CheckpointCorrupt) {
    std::fprintf(stderr, "status=refused detail=%s\n", r.detail.c_str());
    return 4;
  }
  if (r.status != relay::RunStatus::Ok) {
    std::fprintf(stderr, "status=error detail=%s\n", r.detail.c_str());
    return 2;
  }
  std::printf("status=ok\n");
  std::printf("damaged_ranges=%zu\n", r.damaged_ranges.size());
  for (const auto& d : r.damaged_ranges)
    std::printf("range %" PRId64 " %" PRId64 " %s %s %s\n",
                relay::rational_floor(d.range.start_ms),
                relay::rational_floor(d.range.end_ms),
                relay::to_string(d.kind), relay::to_string(d.severity),
                d.note.c_str());
  std::printf("boundary_error_ms=%" PRId64 "\n", r.boundary_error_ms);
  std::printf("resume_ms=%.3f\n", r.resume_ms);
  std::printf("output_sha256=%s\n", r.output_sha256.c_str());
  std::printf("peak_memory_mib=%.3f\n", r.peak_memory_bytes / 1048576.0);
  std::printf("duplicate_packets=%" PRIu64 "\n", r.duplicate_packets);
  std::printf("reordered_packets=%" PRIu64 "\n", r.reordered_packets);
  std::printf("burst_losses=%" PRIu64 "\n", r.burst_losses);
  std::printf("lost_packets=%" PRIu64 "\n", r.lost_packets);
  std::printf("est_bandwidth_kbps=%.1f\n", r.est_bandwidth_kbps);
  return 0;
}

}  // namespace

int main(int argc, char** argv) {
  if (argc < 2) {
    std::fprintf(stderr,
                 "usage: relay_inspect <gen-packets|gen-captions|damage-media|"
                 "analyze> [--key value ...]\n");
    return 2;
  }
  std::string cmd = argv[1];
  Args a = parse_args(argc, argv, 2);
  try {
    if (cmd == "gen-packets") return cmd_gen_packets(a);
    if (cmd == "gen-captions") return cmd_gen_captions(a);
    if (cmd == "damage-media") return cmd_damage_media(a);
    if (cmd == "analyze") return cmd_analyze(a);
  } catch (const std::exception& e) {
    std::fprintf(stderr, "error: %s\n", e.what());
    return 2;
  }
  std::fprintf(stderr, "unknown subcommand: %s\n", cmd.c_str());
  return 2;
}
