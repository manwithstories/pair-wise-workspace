#include "media/cli.hpp"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "media/packet.hpp"
#include "media/pipeline.hpp"
#include "media/sample_generator.hpp"
#include "media/stream_reader.hpp"

namespace media {

namespace {

struct Args {
  std::string command;
  std::string codec = "h264";
  std::uint32_t seed = 41;
  std::uint32_t loss_burst = 0;
  std::vector<std::uint32_t> bandwidth;
  std::string checkpoint_dir;
  std::uint32_t inject_interrupt = 0;
  bool faulty_index = false;
  bool truncated = false;
};

bool parse_bandwidth(const std::string& csv, std::vector<std::uint32_t>& out) {
  out.clear();
  std::size_t pos = 0;
  while (pos < csv.size()) {
    const std::size_t comma = csv.find(',', pos);
    const std::string tok = csv.substr(
        pos, comma == std::string::npos ? std::string::npos : comma - pos);
    if (tok.empty()) return false;
    char* end = nullptr;
    const unsigned long v = std::strtoul(tok.c_str(), &end, 10);
    if (end == tok.c_str() || *end != '\0' || v == 0) return false;
    out.push_back(static_cast<std::uint32_t>(v));
    if (comma == std::string::npos) break;
    pos = comma + 1;
  }
  return !out.empty();
}

// Returns 0 when ok, 64 on any usage error.
int parse_args(const std::vector<std::string>& a, Args& out, std::string& err) {
  if (a.empty()) { err = "missing command"; return 64; }
  out.command = a[0];
  if (out.command != "demo" && out.command != "resume" &&
      out.command != "fixtures") {
    err = "unknown command: " + out.command;
    return 64;
  }
  for (std::size_t i = 1; i < a.size(); ++i) {
    const std::string& k = a[i];
    auto need = [&](std::string& dst) -> bool {
      if (i + 1 >= a.size()) { err = k + " needs a value"; return false; }
      dst = a[++i]; return true;
    };
    std::string val;
    if (k == "--codec") { if (!need(val)) return 64; out.codec = val; }
    else if (k == "--seed") { if (!need(val)) return 64;
      out.seed = static_cast<std::uint32_t>(std::strtoul(val.c_str(), nullptr, 10)); }
    else if (k == "--loss-burst") { if (!need(val)) return 64;
      out.loss_burst = static_cast<std::uint32_t>(std::strtoul(val.c_str(), nullptr, 10)); }
    else if (k == "--bandwidth") { if (!need(val)) return 64;
      if (!parse_bandwidth(val, out.bandwidth)) { err = "bad --bandwidth"; return 64; } }
    else if (k == "--checkpoint-dir") { if (!need(val)) return 64;
      out.checkpoint_dir = val; }
    else if (k == "--inject-interrupt") { if (!need(val)) return 64;
      out.inject_interrupt = static_cast<std::uint32_t>(std::strtoul(val.c_str(), nullptr, 10)); }
    else if (k == "--faulty-index") out.faulty_index = true;
    else if (k == "--truncated") out.truncated = true;
    else { err = "unknown flag: " + k; return 64; }
  }
  if (codec_from_string(out.codec) == CodecType::Unknown) {
    err = "unsupported codec: " + out.codec; return 64;
  }
  return 0;
}

int run_demo_or_resume(const Args& a) {
  PipelineConfig cfg;
  cfg.codec = codec_from_string(a.codec);
  cfg.seed = a.seed;
  cfg.loss_burst = a.loss_burst;
  cfg.bandwidth_kbps = a.bandwidth;
  cfg.checkpoint_dir = a.checkpoint_dir;
  cfg.inject_interrupt = a.inject_interrupt;
  cfg.use_faulty_index = a.faulty_index;
  cfg.truncated_stream = a.truncated;
  PipelineMetrics m;
  const int rc = run_pipeline(cfg, m);
  std::fputs(format_metrics(m).c_str(), stdout);
  if (m.interrupted) std::printf("interrupted=1\n");
  return rc;
}

int run_fixtures(const Args& a) {
  // Generate every damaged fixture and print the locatable error category and
  // the block/frame number it applies to.
  SampleConfig sc;
  sc.codec = codec_from_string(a.codec);
  sc.seed = a.seed;
  const SampleBundle b = generate_sample(sc);
  std::printf("fixture codec=%s seed=%u stream_bytes=%zu\n",
              a.codec.c_str(), a.seed, b.video.size());

  const StreamIndex over = make_oversize_index(b);
  ReadResult r1 = read_stream(b.video, sc.codec, &over);
  for (const auto& e : r1.errors)
    std::printf("oversize_index code=%s frame=%llu\n", error_code_name(e.code),
                static_cast<unsigned long long>(e.sequence));

  const StreamIndex missing = make_missing_entry_index(b);
  ReadResult r2 = read_stream(b.video, sc.codec, &missing);
  for (const auto& e : r2.errors)
    std::printf("missing_entry  code=%s frame=%llu\n", error_code_name(e.code),
                static_cast<unsigned long long>(e.sequence));

  ReadResult r3 = read_stream(make_truncated_stream(b), sc.codec, nullptr,
                              b.video.size());
  for (const auto& e : r3.errors)
    std::printf("truncated      code=%s block=%llu\n", error_code_name(e.code),
                static_cast<unsigned long long>(e.block_id));
  return r1.errors.empty() ? 2 : 0;
}

} // namespace

int run_cli(const std::vector<std::string>& args) {
  if (args.size() == 1 && (args[0] == "--help" || args[0] == "-h")) {
    std::printf(
        "media_replay demo|resume|fixtures [options]\n"
        "  --codec h264|h265|av1  --seed N  --loss-burst N\n"
        "  --bandwidth a,b,c(kbps)  --checkpoint-dir DIR\n"
        "  --inject-interrupt N  --faulty-index  --truncated\n"
        "exit: 0 ok, 2 corrupt input/index, 3 checkpoint, 64 usage\n");
    return 0;
  }
  Args a;
  std::string err;
  const int prc = parse_args(args, a, err);
  if (prc != 0) {
    std::fprintf(stderr, "usage error: %s\n", err.c_str());
    return 64;
  }
  if (a.command == "fixtures") return run_fixtures(a);
  return run_demo_or_resume(a);
}

} // namespace media
