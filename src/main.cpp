// main.cpp — popfix 命令行入口：generate-fixture / analyze / verify 三个子命令
//
// 退出码：0 成功；2 参数或输入错误；3 检测或修复失败；
//         4 校验不通过；5 输出文件写入失败。
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

#include "sample_model.h"

namespace popfix {
namespace {

struct Args {
  std::vector<std::string> positional;
  // 取 --name value；不存在时返回 false。
  bool get(const std::string& name, std::string& out) const {
    for (std::size_t i = 0; i + 1 < tokens.size(); ++i) {
      if (tokens[i] == name) {
        out = tokens[i + 1];
        return true;
      }
    }
    return false;
  }
  bool get(const std::string& name, std::size_t& out) const {
    std::string raw;
    if (!get(name, raw)) return false;
    try {
      std::size_t consumed = 0;
      long long v = std::stoll(raw, &consumed);
      if (consumed != raw.size() || v < 0) return false;
      out = static_cast<std::size_t>(v);
      return true;
    } catch (...) {
      return false;
    }
  }
  bool get(const std::string& name, double& out) const {
    std::string raw;
    if (!get(name, raw)) return false;
    try {
      std::size_t consumed = 0;
      out = std::stod(raw, &consumed);
      return consumed == raw.size();
    } catch (...) {
      return false;
    }
  }
  bool get(const std::string& name, unsigned& out) const {
    std::size_t v = 0;
    if (!get(name, v)) return false;
    out = static_cast<unsigned>(v);
    return true;
  }
  bool has(const std::string& name) const {
    for (const auto& t : tokens)
      if (t == name) return true;
    return false;
  }
  std::vector<std::string> tokens;
};

void print_usage() {
  std::cerr
      << "popfix — 本地 WAV PCM 爆音/削波批处理修复工具\n"
      << "用法:\n"
      << "  popfix generate-fixture --out <wav> [--rate 48000] "
         "[--channels 2] [--frames 96000] [--seed 17]\n"
      << "  popfix analyze --in <wav> [--repair <wav>] [--report <json>]\n"
      << "         [--threshold 0.995] [--context 32] [--threads 4] "
         "[--dry-run]\n"
      << "  popfix verify --original <wav> --repaired <wav> --report <json>\n";
}

}  // namespace
}  // namespace popfix

int main(int argc, char** argv) {
  using namespace popfix;
  Args args;
  args.tokens.assign(argv + 1, argv + argc);
  if (args.tokens.empty()) {
    print_usage();
    return static_cast<int>(ExitCode::UsageOrInputError);
  }
  const std::string cmd = args.tokens.front();
  args.positional = args.tokens;  // 子命令实现按需解析具名参数

  if (cmd == "generate-fixture" || cmd == "analyze" || cmd == "verify") {
    // 具体处理逻辑在后续模块批次接入（wav_io / detector / pipeline /
    // report_writer），批次 1 先保证命令分发骨架可编译。
    std::cerr << "popfix: 子命令 " << cmd << " 将在后续批次实现\n";
    return static_cast<int>(ExitCode::DetectOrRepairFailure);
  }

  print_usage();
  return static_cast<int>(ExitCode::UsageOrInputError);
}
