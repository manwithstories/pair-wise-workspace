// Cross-module pipeline and exception-path tests (Catch2 3.x).
#include <catch2/catch_test_macros.hpp>

#include <algorithm>
#include <filesystem>
#include <fstream>

#include "media/av_sync.hpp"
#include "media/checkpoint_index.hpp"
#include "media/jitter_buffer.hpp"
#include "media/network_replay.hpp"
#include "media/pipeline.hpp"
#include "media/recovery.hpp"
#include "media/sample_generator.hpp"
#include "media/stream_reader.hpp"

using namespace media;

namespace {

SampleBundle make_bundle(CodecType c, std::uint32_t seed = 41) {
  SampleConfig cfg;
  cfg.codec = c;
  cfg.seed = seed;
  return generate_sample(cfg);
}

} // namespace

TEST_CASE("generator is deterministic across codecs", "[generator]") {
  for (auto codec : {CodecType::H264, CodecType::H265, CodecType::AV1}) {
    auto a = make_bundle(codec);
    auto b = make_bundle(codec);
    REQUIRE(a.video == b.video);
    REQUIRE(a.input_hash == b.input_hash);
    REQUIRE(a.frames.size() == a.frames.size());
    REQUIRE_FALSE(a.frames.empty());
    REQUIRE(probe_codec(a.video) == codec);
  }
}

TEST_CASE("reader parses frames and validates index", "[reader]") {
  auto bundle = make_bundle(CodecType::H264);
  auto r = read_stream(bundle.video, CodecType::H264, &bundle.index);
  REQUIRE(r.frames.size() == bundle.frames.size());
  REQUIRE(r.errors.empty());
  REQUIRE(r.validated_index.entries.size() == bundle.index.entries.size());
}

TEST_CASE("damaged indexes produce locatable error codes", "[reader][errors]") {
  auto bundle = make_bundle(CodecType::H265);

  SECTION("out-of-range offset") {
    auto idx = make_oversize_index(bundle);
    auto r = read_stream(bundle.video, CodecType::H265, &idx);
    REQUIRE(any_of(r.errors.begin(), r.errors.end(), [](const ErrorEvent& e) {
      return e.code == ErrorCode::IndexOffsetOutOfRange;
    }));
  }
  SECTION("missing entry is rebuilt by scanning") {
    auto idx = make_missing_entry_index(bundle);
    auto r = read_stream(bundle.video, CodecType::H265, &idx);
    REQUIRE(r.validated_index.entries.size() == bundle.index.entries.size());
    REQUIRE(any_of(r.errors.begin(), r.errors.end(), [](const ErrorEvent& e) {
      return e.code == ErrorCode::IndexMissingEntry;
    }));
  }
  SECTION("truncated stream is flagged") {
    auto bytes = make_truncated_stream(bundle);
    auto r = read_stream(bytes, CodecType::H265, nullptr, bundle.video.size());
    REQUIRE(r.truncated);
  }
}

TEST_CASE("network replay dedups, reorders and wraps safely", "[network]") {
  std::vector<Packet> packets;
  for (std::uint32_t i = 0; i < 120; ++i) {
    Packet p;
    p.frame_index = i;
    p.pts_us = static_cast<std::int64_t>(i) * 33333;
    p.payload.assign(300, static_cast<std::uint8_t>(i));
    packets.push_back(std::move(p));
  }
  NetworkConfig cfg;
  cfg.seed = 41;
  cfg.loss_burst = 7;
  cfg.duplicate_count = 8;
  cfg.reorder_count = 6;
  cfg.bandwidth_kbps = {1800, 700, 2400};

  auto r1 = network_replay(packets, cfg);
  auto r2 = network_replay(packets, cfg);
  REQUIRE(r1.stats.packets_dropped == 7);
  REQUIRE(r1.stats.packets_duplicate == 8);
  REQUIRE(r1.stats.packets_reordered == 6);
  REQUIRE(r1.packets.size() == r2.packets.size());
  for (std::size_t i = 0; i < r1.packets.size(); ++i)
    REQUIRE(r1.packets[i].linear_index == r2.packets[i].linear_index);
}

TEST_CASE("16-bit sequence wrap never deadlocks or falsely dedups",
          "[network][wrap]") {
  std::vector<Packet> packets;
  for (std::uint32_t i = 0; i < 70000; ++i) {
    Packet p;
    p.payload.resize(48);
    packets.push_back(std::move(p));
  }
  NetworkConfig cfg;
  auto r = network_replay(std::move(packets), cfg);
  REQUIRE(r.stats.packets_out == 70000);
  REQUIRE(r.stats.wraps == 1);
}

TEST_CASE("jitter buffer reorders, caps depth and never deadlocks",
          "[jitter]") {
  JitterBuffer jb;
  auto mk = [](std::uint64_t linear) {
    Packet p;
    p.linear_index = linear;
    p.sequence = static_cast<std::uint16_t>(linear);
    p.payload.resize(80);
    return p;
  };
  jb.push(mk(0), 1000);
  jb.push(mk(2), 1000); // identical arrival: must not deadlock
  jb.push(mk(1), 1000);
  std::vector<Gap> gaps;
  REQUIRE(jb.drain(1000, &gaps).empty());
  auto out = jb.drain(1000 + 200000, &gaps);
  REQUIRE(out.size() == 3);
  REQUIRE(out[0].linear_index == 0);
  REQUIRE(out[2].linear_index == 2);
  REQUIRE(jb.stats().target_depth_us >= 20000);
  REQUIRE(jb.stats().target_depth_us <= 120000);
}

TEST_CASE("recovery classifies concealment vs unrecoverable", "[recovery]") {
  RecoveryEngine engine;
  std::vector<Packet> ctx(1);
  Gap short_gap{0, 3, 10, 0};
  auto ok = engine.handle_gap(short_gap, ctx);
  REQUIRE(ok.success);
  REQUIRE(ok.kind == RecoveryKind::ReferenceFrame);

  Gap long_gap{20, 50, 20, 0};
  auto bad = engine.handle_gap(long_gap, ctx);
  REQUIRE(bad.kind == RecoveryKind::Unrecoverable);
  REQUIRE(recovery_rate(engine.stats()) < 1.0);
}

TEST_CASE("AV sync handles missing/jump and resamples rate switch", "[sync]") {
  AVSync sync;
  for (std::uint64_t i = 0; i < 30; ++i)
    sync.push_video(i, static_cast<std::int64_t>(i) * 33333, i == 5);
  AudioBlock blk;
  blk.index = 0;
  blk.pts_us = 0;
  blk.sample_rate = 24000;
  blk.samples.resize(24000 / 50);
  auto item = sync.push_audio(blk, false);
  REQUIRE(item.audio.size() == 960); // resampled to 48 kHz / 20 ms
  REQUIRE(sync.stats().timestamp_missing == 1);
}

TEST_CASE("checkpoint resume ignores tmp and rolls back corruption",
          "[checkpoint]") {
  const std::string dir = "./tmp/test_ckpt";
  std::filesystem::remove_all(dir);
  CheckpointIndex cp(dir);
  for (std::uint32_t i = 0; i < 4; ++i) {
    CheckpointBlock b;
    b.block_id = i;
    b.input_hash = 0x1234;
    b.end_us = 1000;
    b.packet_count = 10;
    std::string err;
    REQUIRE(cp.write_block(b, err));
  }
  CheckpointStatus st;
  REQUIRE(cp.load(st).blocks.size() == 4);

  CheckpointBlock inter;
  inter.block_id = 4;
  inter.input_hash = 0x1234;
  std::string err;
  cp.simulate_interrupt(inter, err);
  REQUIRE(cp.load(st).blocks.size() == 4); // partial ignored

  std::ofstream(dir + "/chunk_00002.txt", std::ios::trunc) << "x";
  auto rolled = cp.load(st);
  REQUIRE(st == CheckpointStatus::RolledBack);
  REQUIRE(rolled.blocks.size() == 2);
  std::filesystem::remove_all(dir);
}

TEST_CASE("end-to-end pipeline meets acceptance thresholds", "[pipeline]") {
  const std::string dir = "./tmp/test_pipe";
  std::filesystem::remove_all(dir);
  PipelineConfig cfg;
  cfg.codec = CodecType::H264;
  cfg.seed = 41;
  cfg.loss_burst = 7;
  cfg.bandwidth_kbps = {1800, 700, 2400};
  cfg.checkpoint_dir = dir;
  PipelineMetrics m;
  REQUIRE(run_pipeline(cfg, m) == 0);
  CHECK(m.sync_error_ms_p95 <= 40.0);
  CHECK(m.recovery_rate >= 0.80);
  CHECK(m.latency_p95_ms <= 250.0);
  CHECK(m.packets_duplicate > 0);
  CHECK(m.packets_reordered > 0);
  std::filesystem::remove_all(dir);
}

TEST_CASE("truncated pipeline exits with code 2", "[pipeline][errors]") {
  PipelineConfig cfg;
  cfg.truncated_stream = true;
  cfg.checkpoint_dir = "./tmp/test_trunc";
  std::filesystem::remove_all(cfg.checkpoint_dir);
  PipelineMetrics m;
  REQUIRE(run_pipeline(cfg, m) == 2);
  REQUIRE(m.input_corrupt);
  std::filesystem::remove_all(cfg.checkpoint_dir);
}
