#pragma once
// 命令行入口共享的退出码约定
namespace h264 {

enum ExitCode {
  EXIT_OK = 0,
  EXIT_USAGE = 2,       // 参数或文件错误
  EXIT_STRICT_FAIL = 3, // strict 模式语法错误/关键字段缺失
  EXIT_TEST_FAIL = 4,   // 测试失败
};

} // namespace h264
