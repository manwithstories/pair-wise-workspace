#include <cstdio>
#include <cstdlib>
#include <string>
#include <string_view>
#include <vector>

#include "fixture.hpp"
#include "pipeline.hpp"
#include "sha256.hpp"
#include "timeline.hpp"

namespace mb {
namespace {

struct Args {
  bool ok = true;
  bool show_help = false;
  std::string command;
  u64 seed = 417;
  bool json = false;
  std::string output_dir;
};

Args parse_args(int argc, char** argv) {
  Args a;
  if (argc < 2) {
    a.ok = false;
    return a;
  }
  a.command = argv[1];
  if (a.command == "help" || a.command == "--help" || a.command == "-h") {
    a.show_help = true;
    return a;
  }
  for (int i = 2; i < argc; ++i) {
    std::string_view arg = argv[i];
    if (arg == "--json") {
      a.json = true;
    } else if (arg == "--seed" && i + 1 < argc) {
      a.seed = static_cast<u64>(std::strtoull(argv[++i], nullptr, 10));
    } else if (arg == "--out" && i + 1 < argc) {
      a.output_dir = argv[++i];
    } else {
      a.ok = false;
    }
  }
  return a;
}

void print_help() {
  std::printf(
      "media_batch - local multi-camera ingest packaging tool\n"
      "usage:\n"
      "  media_batch selftest [--seed N] [--json] [--out DIR]\n"
      "  media_batch process  --seed N [--json] [--out DIR]\n"
      "  media_batch help\n"
      "exit codes: 0 metrics met, 1 metrics not met, 2 bad arguments/config\n");
}

constexpr double kMinRoundtrip = 0.99;
constexpr double kMinKeyframeHit = 0.95;

std::string build_json(const PipelineSummary& s, u64 seed) {
  JsonWriter j;
  j.key("seed");
  j.value_int(static_cast<long long>(seed));
  j.key("roundtrip");
  j.value_double(s.roundtrip_ratio);
  j.key("keyframe_hit");
  j.value_double(s.keyframe_hit_ratio);
  j.key("segments");
  j.value_int(static_cast<long long>(s.segment_count));
  j.key("silence_ms");
  j.value_int(static_cast<long long>(s.silence_ms));
  j.key("recovered");
  j.value_int(static_cast<long long>(s.recovered));
  j.key("failed");
  j.value_int(static_cast<long long>(s.failed));
  j.key("dedup_exact");
  j.value_int(static_cast<long long>(s.dedup.exact_pairs));
  j.key("dedup_candidates");
  j.value_int(static_cast<long long>(s.dedup.candidate_pairs));
  j.key("segments_detail");
  j.begin_array();
  for (const auto& [hash, seg] : s.segment_rows) {
    j.begin_object();
    j.key("id");
    j.value_int(seg.segment_id);
    j.key("track");
    j.value_int(seg.track_id);
    j.key("start_ns");
    j.value_int(static_cast<long long>(seg.start_ns));
    j.key("end_ns");
    j.value_int(static_cast<long long>(seg.end_ns));
    j.key("keyframe_hit");
    j.value_bool(seg.keyframe_hit);
    j.key("truncated");
    j.value_bool(seg.truncated);
    j.key("has_gap");
    j.value_bool(seg.has_gap);
    j.key("sha256");
    j.value_string(Sha256::hex(hash));
    j.end_object();
  }
  j.end_array();
  j.key("errors");
  j.begin_array();
  for (const auto& e : s.errors) {
    j.begin_object();
    j.key("track");
    j.value_int(e.track_id);
    j.key("code");
    j.value_string(e.code);
    j.key("recovery");
    j.value_int(static_cast<int>(e.recovery));
    j.end_object();
  }
  j.end_array();
  return j.finish();
}

int run_pipeline(const Args& args) {
  FixtureOptions opt;
  opt.seed = args.seed;
  FixtureGenerator gen(opt);
  GeneratedFixture fx = gen.generate();

  PipelineConfig cfg;
  cfg.seed = args.seed;
  cfg.emit_json = args.json;
  cfg.output_dir = args.output_dir;
  cfg.fixture = opt;
  Pipeline pipe(cfg);
  PipelineSummary s = pipe.run(fx.capture);
  if (!s.ok) {
    std::fprintf(stderr, "media_batch: pipeline failed to initialise\n");
    return 2;
  }

  // Deterministic per-segment lines: id, track, time range, hash, flags.
  for (const auto& [hash, seg] : s.segment_rows) {
    std::printf(
        "segment id=%u track=%u range=[%lld,%lld] keyframe=%d gap=%d "
        "trunc=%d sha256=%s\n",
        seg.segment_id, seg.track_id,
        static_cast<long long>(seg.start_ns),
        static_cast<long long>(seg.end_ns), int(seg.keyframe_hit),
        int(seg.has_gap), int(seg.truncated), Sha256::hex(hash).c_str());
  }

  // The summary line is machine-greppable and order-stable.
  std::string first_hash =
      s.segment_rows.empty() ? std::string("none")
                             : Sha256::hex(s.segment_rows.front().first);
  std::printf(
      "roundtrip=%.4f keyframe_hit=%.4f segments=%llu segment_sha256=%s "
      "silence_ms=%llu recovered=%llu failed=%llu\n",
      s.roundtrip_ratio, s.keyframe_hit_ratio,
      static_cast<unsigned long long>(s.segment_count), first_hash.c_str(),
      static_cast<unsigned long long>(s.silence_ms),
      static_cast<unsigned long long>(s.recovered),
      static_cast<unsigned long long>(s.failed));

  if (args.json) {
    std::string js = build_json(s, args.seed);
    std::printf("%s\n", js.c_str());
    if (!args.output_dir.empty()) {
      std::string path = args.output_dir + "/summary.json";
      if (!atomic_write_text(path, js))
        std::fprintf(stderr, "warning: could not write %s\n", path.c_str());
    }
  }

  bool metrics = s.roundtrip_ratio >= kMinRoundtrip &&
                 s.keyframe_hit_ratio >= kMinKeyframeHit;
  return metrics ? 0 : 1;
}

}  // namespace
}  // namespace mb

int main(int argc, char** argv) {
  using namespace mb;
  Args args = parse_args(argc, argv);
  if (args.show_help) {
    print_help();
    return 0;
  }
  if (!args.ok || (args.command != "selftest" && args.command != "process")) {
    std::fprintf(stderr, "media_batch: invalid arguments\n");
    print_help();
    return 2;
  }
  return run_pipeline(args);
}
