#include <catch2/catch_test_macros.hpp>
#include "relay/version.hpp"

TEST_CASE("version string is present", "[meta]") {
  REQUIRE(relay::version_string() != nullptr);
}
