// 时钟修复：回绕展开、倒退歧义、重复 PTS、漂移估计与多次回绕单调性。
#include "test_helpers.hpp"

using media_test::mkPkt;

TEST_CASE("33 位 PTS 回绕展开", "[repair]") {
  ClockRepair rep(Rational{1, 90000}, MediaType::Video);
  auto p1 = mkPkt(0, PTS33_MASK, 9000);
  auto p2 = mkPkt(1, 8999, 9000); // 跨回绕边界，相差恰好 9000 滴答
  CHECK(rep.accept(p1) == TimeError::None);
  CHECK(rep.accept(p2) == TimeError::Wrap);
  // 回绕后时间应连续（同一 90kHz 时间基，差 9000 滴答 = 100000us）。
  CHECK(p2.repairedUs - p1.repairedUs == ticksToUs(9000, Rational{1, 90000}));
  CHECK(p2.repairedUs > p1.repairedUs);
  CHECK(rep.summary().wraps == 1);
  CHECK(rep.summary().deliverable);
}

TEST_CASE("小幅倒退判定歧义且不可交付", "[repair]") {
  ClockRepair rep(Rational{1, 90000}, MediaType::Video);
  auto p1 = mkPkt(0, 900000, 3000);
  auto p2 = mkPkt(1, 899000, 3000);
  rep.accept(p1);
  CHECK(rep.accept(p2) == TimeError::Ambiguous);
  CHECK_FALSE(rep.summary().deliverable);
  CHECK(rep.summary().firstBadPacket == 1);
}

TEST_CASE("重复 PTS 可恢复", "[repair]") {
  ClockRepair rep(Rational{1, 90000}, MediaType::Video);
  auto p1 = mkPkt(0, 90000, 3000);
  auto p2 = mkPkt(1, 90000, 3000);
  rep.accept(p1);
  CHECK(rep.accept(p2) == TimeError::DuplicatePts);
  CHECK(p2.duplicate);
  CHECK(p2.repairedUs == p1.repairedUs);
  CHECK(rep.summary().deliverable);
}

TEST_CASE("漂移估计：缺锚点低置信度，充足锚点恢复斜率", "[repair]") {
  ClockRepair rep(Rational{1, 24000}, MediaType::Audio);
  DriftParams empty = rep.estimateDrift();
  CHECK(empty.lowConfidence);

  ClockRepair rep2(Rational{1, 24000}, MediaType::Audio);
  for (int i = 0; i < 100; ++i) {
    double raw = double(i) * 0.01;              // 设备时间
    double expected = raw * (1.0 + 180e-6);     // 应达时间（快 180ppm）
    rep2.addAnchor(raw, expected);
  }
  DriftParams d = rep2.estimateDrift();
  CHECK_FALSE(d.lowConfidence);
  CHECK(d.driftPpm == Approx(180.0).margin(1.0));
  CHECK(std::abs(d.confidence - 1.0) < 0.01);
}

TEST_CASE("连续多次 33 位回绕保持单调", "[repair]") {
  ClockRepair rep(Rational{1, 90000}, MediaType::Video);
  int64_t step = 9000;
  int64_t raw = int64_t(PTS33_MASK) - step;
  int64_t prevUs = 0;
  size_t wraps = 0;
  for (int i = 0; i < int(PTS33_MOD / uint64_t(step)) + 10; ++i) {
    auto p = mkPkt(size_t(i), raw & int64_t(PTS33_MASK), step);
    auto e = rep.accept(p);
    if (e == TimeError::Wrap) ++wraps;
    if (i > 0) CHECK(p.repairedUs > prevUs); // 严格单调
    prevUs = p.repairedUs;
    raw += step;
  }
  CHECK(wraps >= 2);
  CHECK(rep.summary().deliverable);
}
