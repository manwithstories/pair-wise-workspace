#include "relay/types.hpp"

#include <catch2/catch_test_macros.hpp>

using namespace relay;

TEST_CASE("timebase converts frames and microseconds symmetrically", "[timebase]") {
  const Timebase nt{30000, 1001};
  REQUIRE(nt.frames_to_us(30000) == 1'001'000'000);
  REQUIRE(nt.frames_to_us(-30000) == -1'001'000'000);
  REQUIRE(nt.us_to_frames(1'001'000'000) == 30000);
  REQUIRE(nt.us_to_frames(-1'001'000'000) == -30000);

  const Timebase simple{30, 1};
  REQUIRE(simple.frames_to_us(1) == 33'333);
  REQUIRE(simple.frames_to_us(30) == 1'000'000);
}

TEST_CASE("structured errors locate stage track and frame", "[errors]") {
  ErrorSink sink;
  sink.warn("reorder", "cam-b", 7, "duplicate packet");
  sink.fail("verify", "cam-a", 42, "checksum mismatch", "verify<-read<-cam-a");

  REQUIRE(sink.has_warnings());
  REQUIRE(sink.has_errors());
  REQUIRE(sink.exit_code() == static_cast<int>(ExitCode::kCorrupt));
  REQUIRE(sink.count(Severity::kError) == 1);
  const std::string text = sink.events().back().to_string();
  REQUIRE(text.find("track=cam-a") != std::string::npos);
  REQUIRE(text.find("frame=42") != std::string::npos);
  REQUIRE(text.find("verify<-read<-cam-a") != std::string::npos);
}
