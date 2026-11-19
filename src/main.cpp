// 媒体交付流水线 CLI。
// 退出码约定：0 成功；1 参数错误；2 处理失败（致命故障）。
#include <cstring>
#include <iostream>
#include <string>

namespace {
void printUsage() {
  std::cout << "usage:\n"
            << "  media-deliver demo [options]\n"
            << "  media-deliver --help\n";
}
} // namespace

int main(int argc, char** argv) {
  if (argc < 2) {
    printUsage();
    return 1;
  }
  if (std::strcmp(argv[1], "--help") == 0 ||
      std::strcmp(argv[1], "-h") == 0) {
    printUsage();
    return 0;
  }
  if (std::strcmp(argv[1], "demo") != 0) {
    std::cerr << "error: unknown command '" << argv[1] << "'\n";
    return 1;
  }
  // 流水线在后续批次接入。
  std::cerr << "error: pipeline not implemented yet\n";
  return 2;
}
