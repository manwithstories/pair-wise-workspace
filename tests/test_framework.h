#pragma once
// 极简自研单元测试框架：无第三方依赖，失败即抛出并由 main 捕获。
#include <string>
#include <vector>

namespace h264test {

struct TestCase {
  const char *name;
  void (*fn)();
};

std::vector<TestCase> &registry();

// 注册器对象，在静态初始化期把用例塞进 registry()。
struct Registrar {
  Registrar(const char *name, void (*fn)()) { registry().push_back({name, fn}); }
};

int run_all(); // 返回失败用例数（0 即全过）

} // namespace h264test

#define TEST_CASE(name)                                                       \
  static void name();                                                         \
  static ::h264test::Registrar registrar_##name(#name, name);                 \
  static void name()

#define CHECK(cond)                                                           \
  do {                                                                        \
    if (!(cond))                                                              \
      throw std::runtime_error(std::string(__FILE__) + ":" +                  \
                               std::to_string(__LINE__) + " CHECK failed: " + \
                               #cond);                                        \
  } while (0)

#define CHECK_EQ(a, b)                                                        \
  do {                                                                        \
    auto va_ = (a);                                                           \
    auto vb_ = (b);                                                           \
    if (!(va_ == vb_))                                                        \
      throw std::runtime_error(std::string(__FILE__) + ":" +                  \
                               std::to_string(__LINE__) + " CHECK_EQ failed: " \
                               #a " == " #b);                                 \
  } while (0)
