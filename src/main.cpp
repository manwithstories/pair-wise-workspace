// main.cpp — popfix 命令行入口：generate-fixture / analyze / verify 三个子命令
//
// 退出码：0 成功；2 参数或输入错误；3 检测或修复失败；
//         4 校验不通过；5 输出文件写入失败。
#include <iostream>
#include <string>
#include <vector>

#include "commands.h"
#include "sample_model.h"

namespace popfix {
namespace {

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
  if (argc < 2) {
    print_usage();
    return static_cast<int>(ExitCode::UsageOrInputError);
  }
  const std::string cmd = argv[1];
  std::vector<std::string> tokens(argv + 2, argv + argc);

  if (cmd == "generate-fixture")
    return static_cast<int>(cmd_generate_fixture(tokens));
  if (cmd == "analyze")
    return static_cast<int>(cmd_analyze(tokens));
  if (cmd == "verify")
    return static_cast<int>(cmd_verify(tokens));

  print_usage();
  return static_cast<int>(ExitCode::UsageOrInputError);
}
