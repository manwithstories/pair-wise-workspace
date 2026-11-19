// mediaqc -- media QC / repair command line.
//
// Commands:
//   selftest --generate-fixtures --work DIR [--json]
//   batch    --input DIR --output DIR [--resume] [--json]
//   verify   --input DIR [--json]
//
// Exit codes: 0 all verified, 1 at least one asset failed, 2 argument or
// configuration error, 3 output verification failed.
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <map>
#include <string>
#include <vector>

extern "C" {
#include <libavutil/log.h>
}

#include "pipeline.hpp"
#include "util.hpp"

namespace {

void print_usage() {
  std::fputs(
      "usage: mediaqc <command> [options]\n"
      "\n"
      "commands:\n"
      "  selftest --generate-fixtures --work DIR [--json]\n"
      "           Generate boundary fixtures (via ffmpeg) and run the batch\n"
      "           over them.  Exit 0 means every fixture behaved as designed.\n"
      "  batch --input DIR --output DIR [--resume] [--json] [--stop-after-packets N]\n"
      "           Repair every media file in DIR into DIR.  --resume continues\n"
      "           from validated checkpoints and refuses stale ones.\n"
      "  verify --input DIR [--json]\n"
      "           Re-read every output in DIR and re-check its index.\n"
      "\n"
      "exit codes: 0 ok, 1 asset failure, 2 argument/config error,\n"
      "            3 output verification failure\n",
      stderr);
}

struct Args {
  std::string command;
  std::map<std::string, std::string> values;
  std::map<std::string, bool> flags;
};

bool parse_args(int argc, char** argv, Args& args, std::string& err) {
  if (argc < 2) {
    err = "missing command";
    return false;
  }
  args.command = argv[1];
  static const std::map<std::string, bool> kTakesValue = {
      {"--input", true}, {"--output", true}, {"--work", true},
      {"--checkpoints", true}, {"--stop-after-packets", true},
      {"--ffmpeg", true}, {"--max-input-mb", true},
  };
  static const std::map<std::string, bool> kFlags = {
      {"--resume", true}, {"--json", true}, {"--generate-fixtures", true},
      {"--verbose", true}, {"--help", true},
  };
  for (int i = 2; i < argc; ++i) {
    std::string a = argv[i];
    if (a == "-h") a = "--help";
    if (kTakesValue.count(a)) {
      if (i + 1 >= argc) {
        err = "option " + a + " needs a value";
        return false;
      }
      args.values[a] = argv[++i];
    } else if (kFlags.count(a)) {
      args.flags[a] = true;
    } else {
      err = "unknown option: " + a;
      return false;
    }
  }
  return true;
}

bool int_value(const Args& args, const std::string& key, int64_t& out, std::string& err) {
  const auto it = args.values.find(key);
  if (it == args.values.end()) return true;
  try {
    size_t used = 0;
    out = std::stoll(it->second, &used);
    if (used != it->second.size()) {
      err = key + " is not a number: " + it->second;
      return false;
    }
  } catch (...) {
    err = key + " is not a number: " + it->second;
    return false;
  }
  return true;
}

int run(int argc, char** argv) {
  Args args;
  std::string err;
  if (!parse_args(argc, argv, args, err)) {
    std::fprintf(stderr, "mediaqc: %s\n\n", err.c_str());
    print_usage();
    return 2;
  }
  if (args.command == "--help" || args.command == "help" || args.flags["--help"]) {
    print_usage();
    return 0;
  }

  mediaqc::Config config;
  int64_t max_input_mb = 0;
  if (!int_value(args, "--max-input-mb", max_input_mb, err)) {
    std::fprintf(stderr, "mediaqc: %s\n", err.c_str());
    return 2;
  }
  if (max_input_mb > 0) config.limits.max_input_bytes = max_input_mb * 1024 * 1024;
  const bool json = args.flags["--json"] != 0;
  const bool verbose = args.flags["--verbose"] != 0;
  if (verbose) {
    // Keep FFmpeg diagnostics visible for troubleshooting.
    av_log_set_level(AV_LOG_WARNING);
    ::setenv("MEDIAQC_FFMPEG_LOG", "1", 1);
  }

  if (args.command == "selftest") {
    auto it = args.values.find("--work");
    if (it == args.values.end()) {
      std::fprintf(stderr, "mediaqc: selftest needs --work DIR\n");
      return 2;
    }
    mediaqc::SelftestOptions opt;
    opt.config = config;
    opt.work_dir = it->second;
    opt.json = json;
    if (args.values.count("--ffmpeg")) opt.ffmpeg_binary = args.values["--ffmpeg"];
    mediaqc::BatchResult result = mediaqc::run_selftest(opt);
    if (json) {
      std::printf("%s\n", mediaqc::render_json(result, false).c_str());
    } else {
      std::printf("selftest: %lld files, %lld ok, %lld failed (expected failures are "
                  "part of the test)\n",
                  (long long)result.totals.files_total,
                  (long long)result.totals.files_ok,
                  (long long)result.totals.files_failed);
      for (const auto& f : result.files) {
        std::printf("  %-26s %-10s %s\n",
                    std::filesystem::path(f.input_path).filename().string().c_str(),
                    mediaqc::file_status_name(f.status),
                    f.failure_reason.empty() ? "" : f.failure_reason.c_str());
      }
      if (!result.error.empty()) std::printf("selftest note: %s\n", result.error.c_str());
    }
    return result.exit_code;
  }

  if (args.command == "batch") {
    mediaqc::BatchOptions opt;
    opt.config = config;
    auto in = args.values.find("--input");
    auto out = args.values.find("--output");
    if (in == args.values.end() || out == args.values.end()) {
      std::fprintf(stderr, "mediaqc: batch needs --input DIR and --output DIR\n");
      return 2;
    }
    opt.input_dir = in->second;
    opt.output_dir = out->second;
    opt.resume = args.flags["--resume"] != 0;
    opt.json = json;
    opt.verbose = verbose;
    if (args.values.count("--checkpoints")) opt.checkpoint_dir = args.values["--checkpoints"];
    int64_t stop = 0;
    if (!int_value(args, "--stop-after-packets", stop, err)) {
      std::fprintf(stderr, "mediaqc: %s\n", err.c_str());
      return 2;
    }
    opt.stop_after_packets = stop;
    mediaqc::BatchResult result = mediaqc::run_batch(opt);
    if (json) {
      std::printf("%s\n", mediaqc::render_json(result, false).c_str());
    } else {
      for (const auto& f : result.files) {
        std::printf("%-26s %-11s %s\n",
                    std::filesystem::path(f.input_path).filename().string().c_str(),
                    mediaqc::file_status_name(f.status),
                    f.failure_reason.empty() ? "" : f.failure_reason.c_str());
        for (const auto& e : f.errors) {
          std::printf("    [%s/%s] track %d offset %lld",
                      mediaqc::stage_name(e.stage), mediaqc::code_name(e.code), e.track,
                      (long long)e.byte_offset);
          if (e.byte_offset_end > e.byte_offset) {
            std::printf("..%lld", (long long)e.byte_offset_end);
          }
          if (!e.offset_known) std::printf(" (unknown)");
          std::printf(": %s\n", e.detail.c_str());
          if (!e.action.empty()) std::printf("      action: %s\n", e.action.c_str());
        }
      }
      std::printf(
          "batch: files_total=%lld files_ok=%lld files_failed=%lld "
          "first_error_offset_bytes=%lld offset_error_bytes=%lld resume_seconds=%lld "
          "index_entries=%lld decode_errors=%lld\n",
          (long long)result.totals.files_total, (long long)result.totals.files_ok,
          (long long)result.totals.files_failed,
          (long long)result.totals.first_error_offset,
          (long long)result.totals.offset_error_bytes,
          (long long)result.totals.resume_seconds,
          (long long)result.totals.index_entries,
          (long long)result.totals.decode_errors);
      std::printf("batch: output_sha256=%s\n", result.totals.output_sha256.c_str());
      if (!result.error.empty()) std::printf("batch error: %s\n", result.error.c_str());
    }
    return result.exit_code;
  }

  if (args.command == "verify") {
    mediaqc::VerifyOptions opt;
    opt.config = config;
    auto in = args.values.find("--input");
    if (in == args.values.end()) {
      std::fprintf(stderr, "mediaqc: verify needs --input DIR\n");
      return 2;
    }
    opt.input_dir = in->second;
    opt.json = json;
    opt.verbose = verbose;
    mediaqc::BatchResult result = mediaqc::run_verify(opt);
    if (json) {
      std::printf("%s\n", mediaqc::render_json(result, true).c_str());
    } else {
      for (const auto& f : result.files) {
        std::printf("%-26s %-11s index_entries=%lld decode_errors=%lld\n",
                    std::filesystem::path(f.output_path).filename().string().c_str(),
                    mediaqc::file_status_name(f.status),
                    (long long)f.index_entries, (long long)f.decode_errors);
        for (const auto& e : f.errors) {
          std::printf("    [%s/%s] offset %lld: %s\n", mediaqc::stage_name(e.stage),
                      mediaqc::code_name(e.code), (long long)e.byte_offset,
                      e.detail.c_str());
        }
      }
      std::printf(
          "verify: files_total=%lld files_ok=%lld first_error_offset_bytes=%lld "
          "offset_error_bytes=%lld index_entries=%lld decode_errors=%lld\n",
          (long long)result.totals.files_total, (long long)result.totals.files_ok,
          (long long)result.totals.first_error_offset,
          (long long)result.totals.offset_error_bytes,
          (long long)result.totals.index_entries,
          (long long)result.totals.decode_errors);
      std::printf("verify: output_sha256=%s\n", result.totals.output_sha256.c_str());
    }
    return result.exit_code;
  }

  std::fprintf(stderr, "mediaqc: unknown command '%s'\n\n", args.command.c_str());
  print_usage();
  return 2;
}

}  // namespace

int main(int argc, char** argv) { return run(argc, argv); }
