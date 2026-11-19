#include <catch2/catch_test_macros.hpp>
#include <catch2/catch_approx.hpp>
#include "media/time_model.hpp"
#include "media/packet.hpp"
#include "media/stream_pipeline.hpp"

using namespace media;
using Catch::Approx;

TEST_CASE("ticks 与统一微秒时间基互转", "[time_model]") {
  Rational tb{1, 90000}; // 90kHz 滴答
  CHECK(ticksToUs(90000, tb) == 1'000'000);
  CHECK(usToTicks(1'000'000, tb) == 90000);
  CHECK(usToMs(2500) == 2.5);
}

TEST_CASE("33 位掩码常量", "[time_model]") {
  CHECK(PTS33_MOD == (uint64_t(1) << 33));
  CHECK(PTS33_MASK == PTS33_MOD - 1);
}

TEST_CASE("线性拟合与斜率钳制", "[time_model]") {
  std::vector<std::pair<double, double>> pts;
  for (int i = 0; i < 10; ++i)
    pts.push_back({double(i), 2.0 * i + 5.0});
  auto f = fitLine(pts);
  REQUIRE(f.valid);
  CHECK(f.a == Approx(2.0));
  CHECK(f.b == Approx(5.0));
  CHECK(f.r2 == Approx(1.0));
  CHECK(clampSlopePpm(5000.0) == MAX_SLOPE_PPM);
  CHECK(clampSlopePpm(-5000.0) == -MAX_SLOPE_PPM);
  CHECK(clampSlopePpm(180.0) == 180.0);
  CHECK(isFatal(TimeError::TruncatedInput));
  CHECK_FALSE(isFatal(TimeError::Wrap));
}

TEST_CASE("有界队列背压与取消", "[queue]") {
  PacketQueue q(2);
  CHECK(q.push(MediaPacket{}) == PushResult::Ok);
  CHECK(q.push(MediaPacket{}) == PushResult::Ok);
  CHECK(q.peak() == 2);
  q.cancel(TimeError::QueueCancelled);
  CHECK(q.push(MediaPacket{}) == PushResult::Cancelled);
  auto [r, pkt] = q.pop();
  CHECK(r == PopResult::Cancelled);
  CHECK(q.cancelled());
}
