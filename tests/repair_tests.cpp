#include <catch2/catch_test_macros.hpp>
#include <catch2/catch_approx.hpp>
#include "media/time_model.hpp"
#include "media/packet.hpp"
#include "media/stream_pipeline.hpp"
#include "media/manifest.hpp"
#include "media/clock_repair.hpp"

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

TEST_CASE("清单解析与故障路径", "[manifest]") {
  Manifest m;
  auto err = parseManifest(
    "TRACK cam1 video cam1.mkv 1 90000\n"
    "TRACK cam2 video cam2.mkv 1 90000\n"
    "TRACK mic1 audio mic1.wav 1 24000\n"
    "EDIT 0 cam1 1000\n"
    "XFADE 0 cam1 cam2 2000 2500\n", m);
  REQUIRE_FALSE(err);
  REQUIRE(m.tracks.size() == 3);
  CHECK(m.edits.at(0).targetUs == 1'000'000);
  CHECK(m.transitions.at(0).endUs - m.transitions.at(0).startUs == 500'000);

  Manifest bad;
  err = parseManifest("TRACK cam1 video cam1.mkv 0 90000\n", bad);
  CHECK(err);
  CHECK(err.line == 1);

  Manifest dangling;
  err = parseManifest("TRACK cam1 video a.mkv 1 90000\nEDIT 0 ghost 10\n",
                      dangling);
  CHECK(err);
}

TEST_CASE("剪辑点对齐最近有效帧", "[manifest]") {
  std::vector<MediaPacket> pk(3);
  pk[0].index = 0; pk[0].repairedUs = 0;
  pk[1].index = 1; pk[1].repairedUs = 33'333;
  pk[2].index = 2; pk[2].repairedUs = 66'667;
  EditPoint e;
  e.index = 0;
  e.targetUs = 50'000;
  auto st = alignEditPoint(e, pk);
  CHECK(st.kind == BoundaryKind::Snapped);
  CHECK(st.actualUs == 33'333);
  CHECK(st.errorUs == -16'667);
  CHECK(st.nearestPacket == 1);
}

namespace {
MediaPacket mkPkt(size_t gi, int64_t pts, int64_t dur, bool key = false) {
  MediaPacket p;
  p.globalIndex = gi;
  p.index = gi;
  p.rawPts = pts;
  p.rawDts = pts;
  p.durationTicks = dur;
  p.keyframe = key;
  return p;
}
} // namespace

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
