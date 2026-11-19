#include <cstdio>
#include <string>
#include <unordered_map>
#include <vector>

#include "media_archive/audio_sync.h"
#include "media_archive/error.h"
#include "media_archive/keyframe_index.h"
#include "media_archive/recovery_pipeline.h"
#include "media_archive/report.h"
#include "media_archive/sample_generator.h"
#include "media_archive/types.h"

namespace {

using media_archive::Diagnostic;
using media_archive::ExitCode;

void print_help() {
  std::printf(
      "media-archive — long-term media archive recovery tool\n\n"
      "Usage:\n"
      "  media-archive generate --seed N --out DIR [--codec h264|h265|av1]\n"
      "  media-archive recover --codec C --input F --packets F --audio F\n"
      "                        --index F --checkpoint D [--report text|json]\n"
      "                        [--drift-ppm N] [--crash-after N]\n"
      "                        [--max-input-bytes N]\n"
      "  media-archive verify --codec C --index F\n\n"
      "Exit codes: 0 success, 2 usage/argument error,\n"
      "            3 media corruption, 4 checkpoint/write failure\n");
}

std::unordered_map<std::string, std::string> parse_flags(
    const std::vector<std::string>& args, size_t start) {
  std::unordered_map<std::string, std::string> flags;
  for (size_t i = start; i < args.size(); ++i) {
    const std::string& tok = args[i];
    if (tok.rfind("--", 0) != 0) continue;
    const std::string key = tok.substr(2);
    if (i + 1 < args.size() && args[i + 1].rfind("--", 0) != 0)
      flags[key] = args[++i];
    else
      flags[key] = "1";  // boolean flag
  }
  return flags;
}

bool require(const std::unordered_map<std::string, std::string>& flags,
             const std::string& key, std::string* out) {
  auto it = flags.find(key);
  if (it == flags.end() || it->second.empty()) {
    std::fprintf(stderr, "missing required argument --%s\n", key.c_str());
    return false;
  }
  *out = it->second;
  return true;
}

int to_exit(const Diagnostic& d) {
  return static_cast<int>(d.status);
}

int run_generate(const std::vector<std::string>& args) {
  auto flags = parse_flags(args, 1);
  std::string seed_s, out_dir, codec_s = "h264";
  if (!require(flags, "seed", &seed_s) || !require(flags, "out", &out_dir))
    return static_cast<int>(ExitCode::UsageError);
  if (flags.count("codec")) codec_s = flags["codec"];

  media_archive::SampleConfig cfg;
  try {
    cfg.seed = std::stoull(seed_s);
  } catch (...) {
    std::fprintf(stderr, "invalid --seed: %s\n", seed_s.c_str());
    return static_cast<int>(ExitCode::UsageError);
  }
  cfg.codec = media_archive::codec_from_name(codec_s);
  if (cfg.codec == media_archive::CodecType::Unknown) {
    std::fprintf(stderr, "unknown codec: %s\n", codec_s.c_str());
    return static_cast<int>(ExitCode::UsageError);
  }

  media_archive::GeneratedSample sample;
  Diagnostic d = media_archive::generate_sample(cfg, &sample);
  if (!d.ok()) {
    std::fprintf(stderr, "generation failed: %s\n", d.message.c_str());
    return to_exit(d);
  }
  d = media_archive::write_sample_bundle(out_dir, sample);
  if (!d.ok()) {
    std::fprintf(stderr, "write failed: %s\n", d.message.c_str());
    return to_exit(d);
  }
  std::printf("generated %s sample in %s (%zu frames, %llu gap packets)\n",
              media_archive::codec_name(cfg.codec), out_dir.c_str(),
              sample.frames.size(),
              static_cast<unsigned long long>(
                  sample.trace.gap_packet_count));
  return static_cast<int>(ExitCode::Ok);
}

int run_recover(const std::vector<std::string>& args) {
  auto flags = parse_flags(args, 1);
  media_archive::RecoveryOptions opt;
  std::string codec_s, report_s = "json";
  if (!require(flags, "codec", &codec_s) ||
      !require(flags, "input", &opt.input_path) ||
      !require(flags, "packets", &opt.packets_path) ||
      !require(flags, "audio", &opt.audio_path) ||
      !require(flags, "index", &opt.index_path) ||
      !require(flags, "checkpoint", &opt.checkpoint_dir))
    return static_cast<int>(ExitCode::UsageError);

  opt.codec = media_archive::codec_from_name(codec_s);
  if (opt.codec == media_archive::CodecType::Unknown) {
    std::fprintf(stderr, "unknown codec: %s\n", codec_s.c_str());
    return static_cast<int>(ExitCode::UsageError);
  }
  if (flags.count("report")) report_s = flags["report"];
  opt.report_format = media_archive::report_format_from_name(report_s);
  if (flags.count("drift-ppm")) {
    try {
      opt.drift_ppm = std::stod(flags["drift-ppm"]);
    } catch (...) {
      std::fprintf(stderr, "invalid --drift-ppm\n");
      return static_cast<int>(ExitCode::UsageError);
    }
  }
  if (flags.count("crash-after")) {
    try {
      opt.crash_after_index_appends = std::stoull(flags["crash-after"]);
    } catch (...) {
      std::fprintf(stderr, "invalid --crash-after\n");
      return static_cast<int>(ExitCode::UsageError);
    }
  }
  if (flags.count("max-input-bytes")) {
    try {
      opt.limits.max_input_bytes = std::stoull(flags["max-input-bytes"]);
    } catch (...) {
      std::fprintf(stderr, "invalid --max-input-bytes\n");
      return static_cast<int>(ExitCode::UsageError);
    }
  }
  if (flags.count("records-per-block")) {
    try {
      opt.checkpoint.records_per_block =
          static_cast<uint32_t>(std::stoul(flags["records-per-block"]));
    } catch (...) {
      std::fprintf(stderr, "invalid --records-per-block\n");
      return static_cast<int>(ExitCode::UsageError);
    }
  }

  media_archive::PipelineResult result;
  Diagnostic d = media_archive::run_recovery_pipeline(opt, &result);
  // Render the report even on failure so diagnostics remain locatable.
  const std::string rendered =
      media_archive::render_report(result.report, opt.report_format);
  std::fputs(rendered.c_str(), stdout);
  if (!d.ok()) {
    std::fprintf(stderr, "recovery failed (status %d): %s",
                 static_cast<int>(d.status), d.message.c_str());
    if (d.offset >= 0)
      std::fprintf(stderr, " at byte offset %lld",
                   static_cast<long long>(d.offset));
    if (d.block != UINT64_MAX)
      std::fprintf(stderr, " block %llu",
                   static_cast<unsigned long long>(d.block));
    std::fputc('\n', stderr);
    return to_exit(d);
  }
  return static_cast<int>(ExitCode::Ok);
}

int run_verify(const std::vector<std::string>& args) {
  auto flags = parse_flags(args, 1);
  std::string codec_s, index_path;
  if (!require(flags, "codec", &codec_s) ||
      !require(flags, "index", &index_path))
    return static_cast<int>(ExitCode::UsageError);
  const auto codec = media_archive::codec_from_name(codec_s);
  if (codec == media_archive::CodecType::Unknown)
    return static_cast<int>(ExitCode::UsageError);

  std::vector<media_archive::KeyframeEntry> entries;
  media_archive::IndexFileHeader hdr{};
  Diagnostic d =
      media_archive::read_keyframe_index(index_path, codec, &entries, &hdr);
  if (!d.ok()) {
    std::fprintf(stderr, "index verify failed: %s", d.message.c_str());
    if (d.offset >= 0)
      std::fprintf(stderr, " at offset %lld",
                   static_cast<long long>(d.offset));
    std::fputc('\n', stderr);
    return to_exit(d);
  }
  std::printf("index OK: %zu keyframes, stream_size %llu\n",
              entries.size(),
              static_cast<unsigned long long>(hdr.stream_size));
  return static_cast<int>(ExitCode::Ok);
}

}  // namespace

int main(int argc, char** argv) {
  std::vector<std::string> args(argv + 1, argv + argc);
  if (args.empty() || args[0] == "-h" || args[0] == "--help") {
    print_help();
    return args.empty() ? static_cast<int>(ExitCode::UsageError)
                        : static_cast<int>(ExitCode::Ok);
  }
  const std::string& command = args[0];
  if (command == "generate") return run_generate(args);
  if (command == "recover") return run_recover(args);
  if (command == "verify") return run_verify(args);
  std::fprintf(stderr, "unknown command: %s\n", command.c_str());
  print_help();
  return static_cast<int>(ExitCode::UsageError);
}
