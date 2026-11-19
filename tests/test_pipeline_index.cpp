// 解封装、有界队列流水线、关键帧索引与质量报告测试。
#include "test_helpers.hpp"
#include "media/demuxer.hpp"
#include "media/generator.hpp"
#include "media/index_builder.hpp"
#include "media/pipeline.hpp"
#include "media/quality_report.hpp"

using media_test::mkPkt;

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

TEST_CASE("损坏包经有界队列作为可恢复事件不取消管线", "[pipeline]") {
  std::vector<MediaPacket> src(30);
  for (size_t i = 0; i < src.size(); ++i) {
    src[i].globalIndex = i;
    src[i].index = i;
    src[i].corrupt = (i % 5 == 0);
  }
  PacketQueue q(4);
  size_t corruptSeen = 0;
  auto st = runPipeline(q, src, [&](MediaPacket& p) {
    if (p.corrupt) ++corruptSeen;
    return TimeError::None;
  });
  CHECK(st.error == TimeError::None);
  CHECK(st.consumed == 30);
  CHECK(corruptSeen == 6);
  CHECK_FALSE(q.cancelled());
  CHECK(st.peak <= 4);
}

TEST_CASE("端到端流水线截断返回致命错误与首坏包位置", "[pipeline]") {
  auto base = std::filesystem::temp_directory_path() /
              ("md_pipe_" + std::to_string(::getpid()));
  GenConfig cfg;
  cfg.outDir = (base / "g").string();
  cfg.seconds = 4;
  cfg.wrapPts = true;
  cfg.vfr = true;
  GenResult r;
  REQUIRE(generateSynthetic(cfg, r).empty());

  PipelineConfig pc;
  pc.workDir = cfg.outDir;
  pc.manifestPath = r.manifestPath;
  pc.queuePackets = 8;
  pc.truncateAt = 43;
  pc.diagPath = (base / "d.jsonl").string();
  PipelineReport rep;
  auto fatal = runPipeline(pc, rep);
  CHECK(isFatal(fatal));
  CHECK(fatal == TimeError::TruncatedInput);
  CHECK(rep.firstBadPacket == 43);
  std::filesystem::remove_all(base);
}

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

  auto mid = idx.seek(700'000);
  REQUIRE(mid.has_value());
  CHECK(mid->entry.ptsUs == 800'000);
  CHECK(mid->errorUs == 100'000);

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
  std::filesystem::remove(path + ".done");
  std::ofstream trunc(path, std::ios::trunc);
  trunc.close();
  CHECK_FALSE(KeyframeIndex::isComplete(path));
  std::filesystem::remove_all(dir);
}

TEST_CASE("PSNR 与码率偏差计算", "[quality]") {
  PsnrAccumulator ps;
  ps.addFrame(100.0);
  CHECK(ps.frames() == 1);
  CHECK(ps.psnrDb() == Approx(28.13).margin(0.05));
  ps.addFrame(0.0);
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
