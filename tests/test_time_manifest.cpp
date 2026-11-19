// 时间基、错误分类、有界队列与清单解析测试。
#include "test_helpers.hpp"

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
  e.targetUs = 55'000; // 距后帧 66.7ms(11.7ms) 比距前帧 33.3ms(21.7ms) 近
  auto st = alignEditPoint(e, pk);
  CHECK(st.kind == BoundaryKind::Snapped);
  CHECK(st.actualUs == 66'667);
  CHECK(st.errorUs == 11'667);
  CHECK(st.nearestPacket == 2);
}
