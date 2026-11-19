// 命令行入口（骨架）：模块陆续接入，先保证整体可编译。
#include <cstring>
#include <iostream>
#include <string>
#include <vector>

namespace mb {
int run(int argc, char** argv);  // 后续在各模块就绪后实现完整命令分发
}  // namespace mb

namespace mb {
int run(int /*argc*/, char** /*argv*/) {
  // 骨架阶段：完整命令分发将随 demo/report 模块落地。
  std::cerr << "media_batch: command not implemented yet (skeleton build)\n";
  return 5;
}
}  // namespace mb

int main(int argc, char** argv) {
  if (argc >= 2 && std::strcmp(argv[1], "--help") == 0) {
    std::cout
        << "media_batch — 电视台母版批处理工具（骨架阶段）\n"
        << "用法: media_batch <command> [options]\n"
        << "命令: demo（夹具生成+批处理，后续开放）、version\n";
    return 0;
  }
  if (argc >= 2 && std::strcmp(argv[1], "version") == 0) {
    std::cout << "media_batch 0.1.0\n";
    return 0;
  }
  return mb::run(argc, argv);
}
