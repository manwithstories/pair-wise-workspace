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

#include "media/index_builder.hpp"

TEST_CASE("关键帧索引定位与落点误差", "[index]") {
  std::vector<MediaPacket> pk;
  for (int i = 0; i < 12; ++i) {
    MediaPacket p = mkPkt(size_t(i), int64_t(i) * 100'000, 100'000,
                          (i % 4 == 0));
    p.repairedUs = p.rawPts;
    pk.push_back(p);
  }
  KeyframeIndex idx;
  idx.build(pk);
  REQUIRE(idx.entries().size() == 3); // 0,4,8

  auto r = idx.seek(550'000);
  REQUIRE(r.has_value());
  CHECK(r->entry.ptsUs == 400'000);
  CHECK(r->errorUs == -150'000);

  auto early = idx.seek(10);
  REQUIRE(early.has_value());
  CHECK(early->entry.ptsUs == 0);
}

TEST_CASE("索引原子写入：截断临时文件不得误报成功", "[index]") {
  auto dir = std::filesystem::temp_directory_path() /
             ("md_idx_" + std::to_string(::getpid()));
  std::filesystem::create_directories(dir);
  auto path = (dir / "kf.idx").string();

  KeyframeIndex idx;
  CHECK(idx.writeAtomic(path) == TimeError::None);
  CHECK(KeyframeIndex::isComplete(path));

  // 模拟写入中断：删除完成标记并截断数据文件。
  std::filesystem::remove(path + ".done");
  std::ofstream trunc(path, std::ios::trunc);
  trunc.close();
  CHECK_FALSE(KeyframeIndex::isComplete(path));
  std::filesystem::remove_all(dir);
}

#include "media/quality_report.hpp"

TEST_CASE("PSNR 与码率偏差计算", "[quality]") {
  PsnrAccumulator ps;
  ps.addFrame(100.0); // PSNR = 10*log10(65025/100)
  CHECK(ps.frames() == 1);
  CHECK(ps.psnrDb() == Approx(28.13).margin(0.05));
  ps.addFrame(0.0);   // 完美帧 -> 封顶 60dB
  CHECK(ps.psnrDb() > 40.0);
  CHECK(computeBitrateErrorPct(1'800'000, 1'900'000) == Approx(5.5556).margin(0.01));
  CHECK(computeBitrateErrorPct(0, 100) == 0.0);
}

TEST_CASE("JSONL 诊断与报告字段", "[quality]") {
  auto dir = std::filesystem::temp_directory_path() /
             ("md_q_" + std::to_string(::getpid()));
  std::filesystem::create_directories(dir);
  auto path = (dir / "diag.jsonl").string();
  DiagnosticLog log(path);
  REQUIRE(log.open() == TimeError::None);
  REQUIRE(log.append({7, 123, 456, TimeError::Wrap, "wrap"}) == TimeError::None);
  REQUIRE(log.close() == TimeError::None);
  std::ifstream in(path);
  std::string line;
  std::getline(in, line);
  CHECK(line.find("\"packet_index\":7") != std::string::npos);
  CHECK(line.find("\"error\":\"wrap\"") != std::string::npos);

  QualityStats st;
  st.packetsIn = 1000;
  st.targetBitrateBps = 1'800'000;
  st.actualBitrateBps = 1'850'000;
  st.bitrateErrorPct = computeBitrateErrorPct(st.targetBitrateBps,
                                               st.actualBitrateBps);
  st.psnrDb = 33.3;
  auto json = renderReportJson(st);
  for (const char* key : {"packets_in", "wraps", "queue_peak",
                          "target_bitrate_bps", "bitrate_error_pct",
                          "psnr_db", "manifest_alignment_ms", "seek_error_ms"})
    CHECK(json.find(key) != std::string::npos);
  std::filesystem::remove_all(dir);
}

#include "media/gen_patterns.hpp"

TEST_CASE("合成视频时序：漂移/VFR/33 位回绕", "[generator]") {
  GenConfig cfg;
  cfg.seconds = 12;
  cfg.wrapPts = true;
  cfg.vfr = true;
  auto plan = media::gen::videoPlan(cfg, 41, 180.0, 30'000, 97);
  REQUIRE(plan.size() == 360);
  size_t wraps = 0;
  for (const auto& f : plan)
    if (f.wrapped) ++wraps;
  CHECK(wraps == 1);
  CHECK(plan.front().pts > int64_t(PTS33_MASK) - 90000 * 13);
  // VFR：相邻帧时长存在差异。
  bool varied = false;
  for (size_t i = 2; i < plan.size(); ++i)
    if (plan[i].duration != plan[i - 1].duration) varied = true;
  CHECK(varied);
  // 坏包注入确定性。
  size_t corrupt = 0;
  for (const auto& f : plan) if (f.corrupt) ++corrupt;
  CHECK(corrupt >= 2);

  GenConfig flat = cfg;
  flat.vfr = false;
  auto cfr = media::gen::videoPlan(flat, 41, 0.0, 0, 0);
  for (size_t i = 1; i < cfr.size(); ++i)
    CHECK(cfr[i].duration == cfr[1].duration);

  auto audio = media::gen::audioPlan(cfg, 97);
  CHECK(audio.size() == size_t(12 * 24000 / 1024) + 1);
  CHECK(audio.front().samples == 1024);
}

#include "media/generator.hpp"

TEST_CASE("端到端合成：媒体/侧车/清单生成且确定性可复现", "[generator]") {
  auto base = std::filesystem::temp_directory_path() /
              ("md_gen_" + std::to_string(::getpid()));
  GenConfig cfg;
  cfg.outDir = (base / "run1").string();
  cfg.seconds = 4;
  cfg.driftPpm = 180;
  cfg.wrapPts = true;
  cfg.vfr = true;
  cfg.corruptEvery = 37;
  GenResult r1;
  REQUIRE(generateSynthetic(cfg, r1).empty());
  for (const auto& f : r1.files) CHECK(std::filesystem::exists(f));
  for (const auto& f : r1.sidecars) CHECK(std::filesystem::exists(f));
  CHECK(std::filesystem::exists(r1.manifestPath));
  // MKV/WAV 非空且包含真实编码数据。
  CHECK(std::filesystem::file_size(r1.files[0]) > 1000);
  CHECK(std::filesystem::file_size(r1.files[2]) > 1000);

  Sidecar sc;
  std::string err;
  REQUIRE(loadSidecar(r1.sidecars[0], sc, err));
  CHECK(sc.entries.size() == 4u * 30u);
  size_t keyframes = 0, corrupt = 0;
  for (const auto& e : sc.entries) {
    if (e.keyframe) ++keyframes;
    if (e.corrupt) ++corrupt;
  }
  CHECK(keyframes >= 8);
  CHECK(corrupt >= 1);

  // 复现：同参数再生成，侧车逐字节一致。
  GenConfig cfg2 = cfg;
  cfg2.outDir = (base / "run2").string();
  GenResult r2;
  REQUIRE(generateSynthetic(cfg2, r2).empty());
  auto readAll = [](const std::string& p) {
    std::ifstream f(p, std::ios::binary);
    return std::string(std::istreambuf_iterator<char>(f),
                       std::istreambuf_iterator<char>());
  };
  CHECK(readAll(r1.sidecars[0]) == readAll(r2.sidecars[0]));

  Manifest m;
  std::ifstream mf(r1.manifestPath);
  std::string text((std::istreambuf_iterator<char>(mf)),
                   std::istreambuf_iterator<char>());
  REQUIRE_FALSE(parseManifest(text, m));
  CHECK(m.tracks.size() == 3);
  CHECK_FALSE(m.transitions.empty());
  std::filesystem::remove_all(base);
}

#include "media/decoder.hpp"

TEST_CASE("MJPEG 解码与 PSNR 质量度量", "[decoder]") {
  auto base = std::filesystem::temp_directory_path() /
              ("md_dec_" + std::to_string(::getpid()));
  GenConfig cfg;
  cfg.outDir = (base / "g").string();
  cfg.seconds = 3;
  cfg.wrapPts = true;
  cfg.vfr = true;
  cfg.corruptEvery = 11;
  GenResult r;
  REQUIRE(generateSynthetic(cfg, r).empty());

  Sidecar sc;
  std::string err;
  REQUIRE(loadSidecar(r.sidecars[0], sc, err));
  std::vector<bool> corrupt(sc.entries.size(), false);
  for (const auto& e : sc.entries)
    if (e.corrupt) corrupt[e.ordinal] = true;

  auto out = measureVideo(r.files[0], corrupt);
  CHECK(out.framesDecoded > 0);
  CHECK(out.framesCorrupt >= 1);
  CHECK(out.psnr.psnrDb() > 25.0);
  CHECK(out.psnr.psnrDb() < 45.0);

  // 无损坏参考：纯正常素材 PSNR 应更高。
  GenConfig clean = cfg;
  clean.corruptEvery = 0;
  clean.outDir = (base / "clean").string();
  GenResult rc2;
  REQUIRE(generateSynthetic(clean, rc2).empty());
  std::vector<bool> none(90, false);
  auto good = measureVideo(rc2.files[0], none);
  CHECK(good.psnr.psnrDb() >= out.psnr.psnrDb());
  std::filesystem::remove_all(base);
}
