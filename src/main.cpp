#include <chrono>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <iostream>
#include <string>

#include "analyzer.h"
#include "bitreader.h"
#include "diff.h"

namespace {

constexpr int kExitOk = 0;
constexpr int kExitArgs = 2;
constexpr int kExitParse = 3;
constexpr int kExitUnsupported = 4;
constexpr int kExitDrift = 5;

void usage(FILE* out) {
  fprintf(out,
          "h264probe - offline H.264 Annex B structure probe\n"
          "usage:\n"
          "  h264probe analyze --input F [--format json|text] [--out FILE] [--stats]\n"
          "  h264probe diff --left A --right B\n"
          "exit codes: 0 ok, 2 args, 3 parse, 4 unsupported, 5 drift\n");
}

std::string read_file(const std::string& path) {
  FILE* fp = fopen(path.c_str(), "rb");
  if (!fp) {
    throw std::runtime_error("cannot open: " + path);
  }
  std::string data;
  char buf[65536];
  size_t n;
  while ((n = fread(buf, 1, sizeof(buf), fp)) > 0) {
    data.append(buf, n);
  }
  fclose(fp);
  return data;
}

int cmd_analyze(int argc, char** argv) {
  std::string input, output, format = "json";
  bool stats = false;
  for (int i = 0; i < argc; ++i) {
    std::string a = argv[i];
    auto next = [&](const char* name) -> std::string {
      if (i + 1 >= argc) {
        fprintf(stderr, "missing value for %s\n", name);
        exit(kExitArgs);
      }
      return argv[++i];
    };
    if (a == "--input") input = next("--input");
    else if (a == "--out") output = next("--out");
    else if (a == "--format") format = next("--format");
    else if (a == "--stats") stats = true;
    else {
      fprintf(stderr, "unknown argument: %s\n", a.c_str());
      return kExitArgs;
    }
  }
  if (input.empty() || (format != "json" && format != "text")) {
    usage(stderr);
    return kExitArgs;
  }
  try {
    auto t0 = std::chrono::steady_clock::now();
    h264::Report r = h264::analyze_file(input);
    auto t1 = std::chrono::steady_clock::now();
    if (stats) {
      r.has_stats = true;
      r.elapsed_sec = std::chrono::duration<double>(t1 - t0).count();
      r.peak_rss_mb = h264::peak_rss_mb();
    }
    std::string out = format == "json" ? h264::report_to_json(r) : h264::report_to_text(r);
    if (output.empty()) {
      fputs(out.c_str(), stdout);
    } else {
      std::filesystem::path p(output);
      if (p.has_parent_path()) {
        std::filesystem::create_directories(p.parent_path());
      }
      FILE* fp = fopen(output.c_str(), "wb");
      if (!fp) {
        fprintf(stderr, "cannot write: %s\n", output.c_str());
        return kExitArgs;
      }
      fwrite(out.data(), 1, out.size(), fp);
      fclose(fp);
    }
    return kExitOk;
  } catch (const h264::Error& e) {
    fprintf(stderr, "analyze failed: %s\n", e.what());
    return e.code() == h264::ErrCode::kUnsupported ? kExitUnsupported : kExitParse;
  } catch (const std::exception& e) {
    fprintf(stderr, "analyze failed: %s\n", e.what());
    return kExitParse;
  }
}

int cmd_diff(int argc, char** argv) {
  std::string left, right;
  for (int i = 0; i < argc; ++i) {
    std::string a = argv[i];
    if (a == "--left" && i + 1 < argc) left = argv[++i];
    else if (a == "--right" && i + 1 < argc) right = argv[++i];
    else {
      fprintf(stderr, "unknown or incomplete argument: %s\n", a.c_str());
      return kExitArgs;
    }
  }
  if (left.empty() || right.empty()) {
    usage(stderr);
    return kExitArgs;
  }
  std::string lj, rj;
  try {
    lj = read_file(left);
    rj = read_file(right);
  } catch (const std::exception& e) {
    fprintf(stderr, "diff failed: %s\n", e.what());
    return kExitArgs;
  }
  try {
    auto diffs = h264::diff_reports(lj, rj);
    if (diffs.empty()) {
      printf("no drift: reports identical\n");
      return kExitOk;
    }
    for (const auto& d : diffs) {
      printf("%s: %s -> %s\n", d.path.c_str(), d.left.c_str(), d.right.c_str());
    }
    printf("%zu drifted field(s)\n", diffs.size());
    return kExitDrift;
  } catch (const h264::Error& e) {
    fprintf(stderr, "diff failed: %s\n", e.what());
    return kExitParse;
  }
}

}  // namespace

int main(int argc, char** argv) {
  if (argc < 2) {
    usage(stderr);
    return kExitArgs;
  }
  std::string cmd = argv[1];
  if (cmd == "--help" || cmd == "-h") {
    usage(stdout);
    return kExitOk;
  }
  if (cmd == "analyze") {
    return cmd_analyze(argc - 2, argv + 2);
  }
  if (cmd == "diff") {
    return cmd_diff(argc - 2, argv + 2);
  }
  usage(stderr);
  return kExitArgs;
}
