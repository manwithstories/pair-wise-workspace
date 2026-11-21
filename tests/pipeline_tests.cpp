#include <catch2/catch_test_macros.hpp>

#include "relay/frame_decoder.hpp"
#include "relay/frame_record.hpp"
#include "relay/index_builder.hpp"
#include "relay/media_clock.hpp"
#include "relay/segment_writer.hpp"
#include "relay/stream_pipeline.hpp"

#include <filesystem>
#include <string>

namespace fs = std::filesystem;
using namespace relay;

namespace {
std::string tmp_dir(const std::string& name) {
  std::string p = (fs::temp_directory_path() / ("relay_test_" + name)).string();
  std::error_code ec;
  fs::remove_all(p, ec);
  fs::create_directories(p, ec);
  return p;
}

FrameRecord make_frame(uint64_t seq, int64_t clock, bool keyframe = false) {
  FrameRecord r;
  r.frame_seq = seq;
  r.node_clock = clock;
  r.time_base = {1, 90000};
  r.keyframe = keyframe;
  r.payload = encode_test_pattern_png(32, 32, seq);
  r.checksum = record_checksum(r);
  return r;
}

std::string write_container(const std::string& dir, const std::string& name,
                            const std::vector<FrameRecord>& recs) {
  std::string path = dir + "/" + name;
  FrameContainerWriter w(path);
  REQUIRE(w.write_header(recs.size()));
  for (const auto& r : recs) REQUIRE(w.write_record(r));
  REQUIRE(w.write_trailer());
  return path;
}
} // namespace

TEST_CASE("frame_record: container roundtrip preserves fields", "[record]") {
  auto dir = tmp_dir("record_roundtrip");
  std::vector<FrameRecord> recs = {make_frame(0, 100, true),
                                   make_frame(1, 4600, false)};
  auto path = write_container(dir, "ok.frames", recs);
  FrameContainerReader r(path);
  REQUIRE(r.open().ok());
  REQUIRE(r.declared_frames() == 2);
  FrameRecord got;
  REQUIRE(r.next(got));
  CHECK(got.frame_seq == 0);
  CHECK(got.node_clock == 100);
  CHECK(got.keyframe);
  CHECK(got.time_base == TimeBase{1, 90000});
  CHECK(checksum_matches(got));
  REQUIRE(r.next(got));
  CHECK(got.frame_seq == 1);
  CHECK_FALSE(r.next(got)); // clean trailer
  CHECK(r.last_error().ok());
}

TEST_CASE("frame_record: rejects truncation, bad length, unknown time base",
          "[record][corrupt]") {
  auto dir = tmp_dir("record_corrupt");
  auto path = write_container(dir, "t.frames", {make_frame(0, 0, true)});
  auto size = fs::file_size(path);

  SECTION("truncated tail reports first error offset") {
    fs::resize_file(path, size - 4); // cut into the trailer only
    FrameContainerReader r(path);
    REQUIRE(r.open().ok());
    FrameRecord got;
    REQUIRE(r.next(got)); // first record intact
    CHECK_FALSE(r.next(got));
    CHECK(r.last_error().error == ParseError::TruncatedTail);
    CHECK(r.last_error().error_offset > 0);
  }
  SECTION("bad magic is a structural error at offset 0") {
    std::FILE* f = std::fopen(path.c_str(), "r+b");
    std::fwrite("XX", 1, 2, f);
    std::fclose(f);
    FrameContainerReader r(path);
    auto res = r.open();
    CHECK(res.error == ParseError::BadMagic);
    CHECK(res.error_offset == 0);
  }
  SECTION("unknown time base is rejected per record") {
    auto dir2 = tmp_dir("record_bad_tb");
    FrameRecord bad = make_frame(0, 0, true);
    bad.time_base = {7, 13};
    bad.checksum = record_checksum(bad);
    auto p2 = write_container(dir2, "tb.frames", {bad, make_frame(1, 100)});
    FrameContainerReader r2(p2);
    REQUIRE(r2.open().ok());
    FrameRecord got;
    CHECK_FALSE(r2.next(got));
    CHECK(r2.last_error().error == ParseError::UnknownTimeBase);
    REQUIRE(r2.next(got)); // stream continues after rejection
    CHECK(got.frame_seq == 1);
  }
}

TEST_CASE("clock repair: wrap, duplicate, backward, drift", "[clock]") {
  ClockRepairConfig cfg;
  cfg.wrap_modulus = 1 << 16;
  cfg.max_drift_ticks = 90000;
  ClockRepairer rep(cfg);
  const TimeBase tb{1, 90000};

  CHECK(rep.observe(60000, tb).error == ClockError::None);
  CHECK(rep.observe(63000, tb).error == ClockError::None);
  CHECK(rep.observe(63000, tb).error == ClockError::Duplicate);
  CHECK(rep.observe(61000, tb).error == ClockError::Backward);
  // Counter wraps past 65536: 100 + 65536 unwrapped.
  auto wrapped = rep.observe(100, tb);
  CHECK(wrapped.error == ClockError::None);
  CHECK(wrapped.unwrapped == 65636);
  CHECK(rep.repaired_wraps() == 1);
  // Extreme jump beyond the drift limit is flagged, value preserved.
  auto drift = rep.observe(65636 + 90000LL * 3600, tb);
  CHECK(drift.error == ClockError::DriftExceeded);
  CHECK(drift.repaired == drift.unwrapped);
}

TEST_CASE("decoder: valid png passes, garbage is isolated", "[decoder]") {
  CHECK(decode_validate(encode_test_pattern_png(32, 32, 7)).ok);
  std::vector<uint8_t> junk(128, 0xAB);
  CHECK_FALSE(decode_validate(junk).ok);
}

TEST_CASE("index: VFR gaps, duplicates, keyframe hits", "[index]") {
  IndexBuilder idx;
  ClockRepairer rep(ClockRepairConfig{});
  auto add = [&](uint64_t seq, int64_t clock, bool kf) {
    FrameRecord r = make_frame(seq, clock, kf);
    idx.add(r, rep.observe(clock, r.time_base), 0, seq * 100);
  };
  add(0, 0, true);
  add(1, 1500, false); // VFR: irregular intervals
  add(1, 1500, false); // duplicate delivery
  add(4, 7000, false); // seqs 2-3 missing -> gap
  add(5, 9300, true);

  CHECK(idx.duplicate_count() == 1);
  REQUIRE(idx.gaps().size() == 1);
  CHECK(idx.gaps()[0].after_seq == 1);
  CHECK(idx.gaps()[0].missing == 2);
  CHECK(idx.keyframe_hit_rate() == 1.0);
  const IndexEntry* e = idx.find_by_seq(4);
  REQUIRE(e != nullptr);
  CHECK(e->keyframe_seq == 0);
  CHECK(idx.validate_entry(*e, 5 * 100));
  CHECK_FALSE(idx.validate_entry(*e, 4 * 100)); // offset out of bounds
  CHECK(idx.find_by_time(0.08)->frame_seq == 4);
}

TEST_CASE("segment writer: crash recovery keeps only verified segments",
          "[segment][recovery]") {
  auto dir = tmp_dir("segment_recovery");
  {
    SegmentWriter w(dir, 4);
    for (uint64_t i = 0; i < 10; ++i)
      REQUIRE(w.write_frame(make_frame(i, i * 3000, i % 4 == 0)) !=
              UINT64_MAX);
    // destructor drops the open third segment: simulated crash
  }
  CHECK(fs::exists(dir + "/segment_00002.bin.tmp"));

  SegmentWriter w2(dir, 4);
  REQUIRE(w2.recover());
  CHECK(w2.completed().size() == 2); // only sealed segments resume
  CHECK(w2.current_segment_id() == 2);
  CHECK_FALSE(fs::exists(dir + "/segment_00002.bin.tmp"));
  // Resume: new frames land in segment 2, old segments untouched.
  REQUIRE(w2.write_frame(make_frame(10, 30000, false)) == 0);
  REQUIRE(w2.finalize());
  CHECK(w2.completed().size() == 3);
  CHECK(SegmentWriter::verify_file(w2.completed()[2].path,
                                   w2.completed()[2].sha256,
                                   w2.completed()[2].bytes));
}

TEST_CASE("pipeline: bounded queue, isolation, structural failure",
          "[pipeline]") {
  auto dir = tmp_dir("pipeline");

  SECTION("queue capacity bounds buffering; corrupt frames isolated") {
    std::vector<FrameRecord> recs;
    for (uint64_t i = 0; i < 500; ++i) {
      FrameRecord r = make_frame(i, i * 3000, i % 30 == 0);
      if (i == 100 || i == 300) r.payload[0] ^= 0xFF; // checksum mismatch
      recs.push_back(r);
    }
    auto path = write_container(dir, "mix.frames", recs);
    PipelineConfig cfg;
    cfg.queue_capacity = 16;
    cfg.segment_frames = 50;
    StreamPipeline p(cfg, dir + "/out");
    REQUIRE(p.run(path) == PipelineStatus::Ok);
    CHECK(p.stats().peak_buffered_frames <= 16);
    CHECK(p.stats().backpressure_events > 0);
    CHECK(p.stats().checksum_isolated == 2);
    CHECK(p.stats().frames_written == 498);
    CHECK(p.index().keyframe_hit_rate() == 1.0);
  }

  SECTION("truncated tail fails with first error offset") {
    auto path = write_container(dir, "trunc.frames",
                                {make_frame(0, 0, true), make_frame(1, 100)});
    fs::resize_file(path, fs::file_size(path) - 4); // partial trailer
    StreamPipeline p(PipelineConfig{}, dir + "/out2");
    CHECK(p.run(path) == PipelineStatus::MediaError);
    CHECK(p.structural_error().error == ParseError::TruncatedTail);
    CHECK(p.structural_error().error_offset > 0);
  }

  SECTION("unknown time base rejected, stream continues") {
    FrameRecord bad = make_frame(5, 5 * 3000);
    bad.time_base = {7, 13};
    bad.checksum = record_checksum(bad);
    std::vector<FrameRecord> recs;
    for (uint64_t i = 0; i < 10; ++i)
      recs.push_back(i == 5 ? bad : make_frame(i, i * 3000, i == 0));
    auto path = write_container(dir, "tb.frames", recs);
    StreamPipeline p(PipelineConfig{}, dir + "/out3");
    REQUIRE(p.run(path) == PipelineStatus::Ok);
    CHECK(p.stats().parse_rejected == 1);
    CHECK(p.stats().frames_written == 9);
  }
}
