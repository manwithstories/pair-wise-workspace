#include <catch2/catch_test_macros.hpp>

// Full coverage lands in the test batch; this stub keeps the target buildable.
TEST_CASE("pipeline target placeholder", "[smoke]") {
  REQUIRE(1 + 1 == 2);
}
