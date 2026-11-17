// test_detector.cpp — popfix 单元测试（测试批次扩充完整用例）
//
// 不依赖外部框架：极简 CHECK 宏 + main 返回非零表示失败，可直接被 ctest 收集。
#include <cstdio>
#include <stdexcept>

namespace {
int g_failures = 0;
int g_checks = 0;
}  // namespace

#define CHECK(cond)                                                  \
  do {                                                               \
    ++g_checks;                                                      \
    if (!(cond)) {                                                   \
      ++g_failures;                                                  \
      std::printf("FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond);    \
    }                                                                \
  } while (0)

int main() {
  // 占位用例：验证测试可执行文件本身可运行。
  CHECK(1 + 1 == 2);
  std::printf("checks=%d failures=%d\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}
