#include "test_framework.h"

#include <cstdio>
#include <exception>
#include <string>

namespace h264test {

std::vector<TestCase> &registry() {
  static std::vector<TestCase> cases;
  return cases;
}

int run_all() {
  int failed = 0;
  for (const auto &tc : registry()) {
    try {
      tc.fn();
      std::printf("[ PASS ] %s\n", tc.name);
    } catch (const std::exception &e) {
      ++failed;
      std::printf("[ FAIL ] %s: %s\n", tc.name, e.what());
    } catch (...) {
      ++failed;
      std::printf("[ FAIL ] %s: unknown exception\n", tc.name);
    }
  }
  std::printf("%zu cases, %d failed\n", registry().size(), failed);
  return failed;
}

} // namespace h264test

int main() {
  // 测试失败按约定退出码 4（见需求：测试失败为 4）。
  const int failed = ::h264test::run_all();
  return failed == 0 ? 0 : 4;
}
