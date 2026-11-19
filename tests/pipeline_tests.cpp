#include <catch2/catch_test_macros.hpp>

// 骨架测试：验证 Catch2 与构建链可用，后续批次追加真实用例。
TEST_CASE("skeleton: test runner alive", "[skeleton]") {
  REQUIRE(1 + 1 == 2);
}
