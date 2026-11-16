#include <cstring>
#include <fstream>
#include <iostream>
#include <sstream>
#include <string>

#include "exit_codes.h"
#include "fixture_writer.h"
#include "gop_analyzer.h"
#include "nal_scanner.h"
#include "report_writer.h"

using namespace h264;

namespace {

void printUsage(std::ostream &o) {
  o << "h264inspect — H.264 Annex B 码流结构分析器\n\n"
    << "usage:\n"
    << "  h264inspect analyze --input <file|-> [--mode strict|tolerant]\n"
    << "                      [--format table|json]\n"
    << "  h264inspect generate --output <file> --frames <n>\n"
    << "                       --slices <n> [--interlaced]\n\n"
    << "  --input - 表示从 stdin 读取码流\n";
}

// 简易 --key value / --flag 解析
struct Args {
  std::string input;
  std::string output;
  std::string mode = "tolerant";
  std::string format = "table";
  uint32_t frames = 24;
  uint32_t slices = 3;
  bool interlaced = false;
};

bool parseArgs(int argc, char **argv, const std::string &sub, Args &a,
               std::string &err) {
  for (int i = 2; i < argc; ++i) {
    std::string key = argv[i];
    auto need = [&](std::string &dst) -> bool {
      if (i + 1 >= argc) {
        err = key + " requires a value";
        return false;
      }
      dst = argv[++i];
      return true;
    };
    if (key == "--input") {
      if (!need(a.input))
        return false;
    } else if (key == "--output") {
      if (!need(a.output))
        return false;
    } else if (key == "--mode") {
      if (!need(a.mode))
        return false;
    } else if (key == "--format") {
      if (!need(a.format))
        return false;
    } else if (key == "--frames") {
      std::string v;
      if (!need(v))
        return false;
      a.frames = static_cast<uint32_t>(std::stoul(v));
    } else if (key == "--slices") {
      std::string v;
      if (!need(v))
        return false;
      a.slices = static_cast<uint32_t>(std::stoul(v));
    } else if (key == "--interlaced") {
      a.interlaced = true;
    } else {
      err = "unknown argument: " + key;
      return false;
    }
  }

  if (sub == "analyze") {
    if (a.input.empty()) {
      err = "analyze requires --input";
      return false;
    }
    if (a.mode != "strict" && a.mode != "tolerant") {
      err = "--mode must be strict or tolerant";
      return false;
    }
    if (a.format != "table" && a.format != "json") {
      err = "--format must be table or json";
      return false;
    }
  } else { // generate
    if (a.output.empty()) {
      err = "generate requires --output";
      return false;
    }
    if (a.frames == 0 || a.slices == 0) {
      err = "frames and slices must be positive";
      return false;
    }
  }
  return true;
}

int runAnalyze(const Args &a) {
  const bool strict = (a.mode == "strict");

  // 流式扫描：stdin 或文件均按分块读取，每发现一个 NAL 立即分析，
  // 不累积全部 NAL，驻留内存仅随单个 NAL 大小增长。
  std::ifstream file;
  std::istream *in = &std::cin;
  std::string source = a.input;
  if (a.input != "-") {
    file.open(a.input, std::ios::binary);
    if (!file) {
      std::cerr << "error: cannot open input: " << a.input << "\n";
      return EXIT_USAGE;
    }
    in = &file;
  } else {
    source = "stdin";
  }

  GopAnalyzer analyzer(strict);
  bool hard_fail = false;
  NalScanner::scanStreamCb(*in, [&](const NalUnit &nal) {
    if (!analyzer.processNal(nal))
      hard_fail = true; // strict 下记录但继续，最终以非零退出
  });
  AnalysisResult result = analyzer.finish();

  const ReportFormat fmt =
      a.format == "json" ? ReportFormat::JSON : ReportFormat::TABLE;
  ReportWriter::write(std::cout, result, fmt, strict, source);

  // strict：出现语法失败或缺少有效 SPS（关键字段缺失）即退出 3
  if (strict && (hard_fail || result.parse_failure_count > 0 ||
                 result.sps.empty()))
    return EXIT_STRICT_FAIL;
  return EXIT_OK;
}

int runGenerate(const Args &a) {
  FixtureParams p;
  p.frames = a.frames;
  p.slices_per_frame = a.slices;
  p.interlaced = a.interlaced;
  std::vector<uint8_t> data = generateFixture(p);
  if (!FixtureWriter::saveBytes(a.output, data)) {
    std::cerr << "error: cannot write output: " << a.output << "\n";
    return EXIT_USAGE;
  }
  std::cerr << "wrote " << data.size() << " bytes to " << a.output << "\n";
  return EXIT_OK;
}

} // namespace

int main(int argc, char **argv) {
  if (argc < 2) {
    printUsage(std::cerr);
    return EXIT_USAGE;
  }
  const std::string sub = argv[1];
  if (sub == "-h" || sub == "--help" || sub == "help") {
    printUsage(std::cout);
    return EXIT_OK;
  }
  if (sub != "analyze" && sub != "generate") {
    std::cerr << "error: unknown subcommand: " << sub << "\n\n";
    printUsage(std::cerr);
    return EXIT_USAGE;
  }

  Args a;
  std::string err;
  try {
    if (!parseArgs(argc, argv, sub, a, err)) {
      std::cerr << "error: " << err << "\n\n";
      printUsage(std::cerr);
      return EXIT_USAGE;
    }
  } catch (const std::exception &e) {
    std::cerr << "error: invalid numeric argument: " << e.what() << "\n";
    return EXIT_USAGE;
  }

  return sub == "analyze" ? runAnalyze(a) : runGenerate(a);
}
