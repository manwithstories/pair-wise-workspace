#include <fstream>
#include <unistd.h>
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

#include "media/demuxer.hpp"
#include <filesystem>

TEST_CASE("侧车解析、截断与零时长", "[demuxer]") {
  auto dir = std::filesystem::temp_directory_path() /
             ("md_demux_" + std::to_string(::getpid()));
  std::filesystem::create_directories(dir);
  auto scPath = (dir / "cam1.sidecar").string();
  {
    std::ofstream f(scPath);
    f << "META 1 90000 0 5\n";
    for (int i = 0; i < 5; ++i)
      f << "PKT " << i << " cam1 video " << i * 3000 << " 3000 "
        << (i % 3 == 0 ? 1 : 0) << " " << (i == 2 ? 1 : 0) << " "
        << i * 1024 << "\n";
  }
  Sidecar sc;
  std::string err;
  REQUIRE(loadSidecar(scPath, sc, err));
  CHECK(sc.timeBase.den == 90000);
  CHECK(sc.entries.size() == 5);
  CHECK(sc.entries[2].corrupt);

  auto full = demuxTrack("cam1", sc, 0);
  CHECK(full.packets.size() == 5);
  CHECK(full.fatal == TimeError::None);

  auto cut = demuxTrack("cam1", sc, 3);
  CHECK(cut.packets.size() == 3);
  CHECK(cut.fatal == TimeError::TruncatedInput);
  CHECK(cut.firstBadPacket == 3);

  Sidecar empty;
  empty.timeBase = {1, 90000};
  auto zero = demuxTrack("ghost", empty, 0);
  CHECK(zero.fatal == TimeError::ZeroDuration);
  std::filesystem::remove_all(dir);
}

TEST_CASE("流水线背压峰值受限且下游致命错误取消生产者", "[pipeline]") {
  std::vector<MediaPacket> src(200);
  for (size_t i = 0; i < src.size(); ++i) {
    src[i].globalIndex = i;
    src[i].index = i;
  }
  PacketQueue q(8);
  size_t seen = 0;
  auto stats = runPipeline(q, src, [&](MediaPacket& p) {
    ++seen;
    if (p.globalIndex == 50) return TimeError::BackwardIllegal;
    return TimeError::None;
  });
  CHECK(stats.error == TimeError::BackwardIllegal);
  CHECK(stats.firstBadPacket == 50);
  CHECK(seen == 51);
  CHECK(stats.peak <= 8);
  CHECK(q.cancelled());

  PacketQueue q2(4);
  auto ok = runPipeline(q2, src, [](MediaPacket&) { return TimeError::None; });
  CHECK(ok.error == TimeError::None);
  CHECK(ok.consumed == 200);
  CHECK(ok.peak <= 4);
}
