#include <catch2/catch_test_macros.hpp>

#include <cstdint>
#include <vector>

#include "keyframe_index.hpp"
#include "sha256.hpp"

using namespace mfs;

namespace {

constexpr int64_t kNominal = 33333;

RepairedFrame make_frame(uint64_t seq, bool keyframe) {
  RepairedFrame frame;
  frame.seq = seq;
  frame.ts_us = static_cast<int64_t>(seq) * kNominal;
  frame.keyframe = keyframe;
  frame.decoded = true;
  frame.frame_sha256 = Sha256::hash("frame-" + std::to_string(seq));
  frame.source_offset = seq * 100;
  return frame;
}

std::vector<RepairedFrame> make_stream(size_t count, size_t keyframe_every) {
  std::vector<RepairedFrame> frames;
  for (size_t i = 0; i < count; ++i) {
    frames.push_back(make_frame(i, i % keyframe_every == 0));
  }
  return frames;
}

}  // namespace

TEST_CASE("segments start at keyframes", "[index]") {
  const auto frames = make_stream(10, 4);  // keyframes at 0, 4, 8
  KeyframeIndex index(kStreamTimeBase);
  REQUIRE(index.build(frames));
  REQUIRE(index.entries().size() == 3);
  CHECK(index.entries()[0].keyframe_seq == 0);
  CHECK(index.entries()[1].keyframe_seq == 4);
  CHECK(index.entries()[2].keyframe_seq == 8);
  CHECK(index.entries()[0].frame_count == 4);
  CHECK(index.entries()[1].frame_count == 4);
  CHECK(index.entries()[2].frame_count == 2);
  CHECK_FALSE(index.truncated());
}

TEST_CASE("seek resolves timestamps to the containing segment", "[index]") {
  const auto frames = make_stream(10, 4);
  KeyframeIndex index(kStreamTimeBase);
  REQUIRE(index.build(frames));

  const SeekResult first = index.seek(0);
  REQUIRE(first.found);
  CHECK(first.keyframe_seq == 0);

  const SeekResult middle = index.seek(frames[5].ts_us);
  REQUIRE(middle.found);
  CHECK(middle.keyframe_seq == 4);
  CHECK(middle.segment_start_us == frames[4].ts_us);
  CHECK(middle.time_base == kStreamTimeBase);

  const SeekResult last = index.seek(frames[9].ts_us + 5);
  REQUIRE(last.found);
  CHECK(last.keyframe_seq == 8);

  const SeekResult before = index.seek(-1);
  CHECK_FALSE(before.found);
}

TEST_CASE("index time base matches the frame time base", "[index]") {
  auto frames = make_stream(8, 2);
  for (RepairedFrame& frame : frames) {
    frame.time_base = kStreamTimeBase;
    frame.pts = microseconds_to_ticks(frame.ts_us, kStreamTimeBase);
  }
  KeyframeIndex index(kStreamTimeBase);
  REQUIRE(index.build(frames));
  for (const IndexEntry& entry : index.entries()) {
    CHECK(entry.time_base == kStreamTimeBase);
  }
  const SeekResult hit = index.seek(frames[3].ts_us);
  REQUIRE(hit.found);
  CHECK(hit.time_base == index.time_base());
  // pts of the keyframe at 2 frames * 1/30 s in 1/90000 is 6000 ticks.
  CHECK(index.entries()[1].pts == 6000);
}

TEST_CASE("undecodable frames do not open segments", "[index]") {
  auto frames = make_stream(6, 2);
  frames[4].decoded = false;  // keyframe at 4 fails to decode
  KeyframeIndex index(kStreamTimeBase);
  REQUIRE(index.build(frames));
  // Keyframe at 4 is skipped, so the segment opened at 2 extends to the end.
  CHECK(index.entries().size() == 2);
  CHECK(index.entries()[1].keyframe_seq == 2);
}

TEST_CASE("a stream with no keyframe still yields a segment", "[index]") {
  auto frames = make_stream(4, 1000);
  KeyframeIndex index(kStreamTimeBase);
  REQUIRE(index.build(frames));
  REQUIRE(index.entries().size() == 1);
  CHECK(index.entries()[0].keyframe_seq == 0);
  CHECK(index.entries()[0].frame_count == 4);
}

TEST_CASE("segment digests change with frame content", "[index]") {
  auto frames = make_stream(4, 2);
  KeyframeIndex index(kStreamTimeBase);
  REQUIRE(index.build(frames));
  const auto digest = index.entries()[0].segment_sha256;

  auto modified = frames;
  modified[1].frame_sha256 = Sha256::hash("different");
  KeyframeIndex other(kStreamTimeBase);
  REQUIRE(other.build(modified));
  CHECK(other.entries()[0].segment_sha256 != digest);
}

TEST_CASE("segment budget truncates without discarding earlier segments",
          "[index]") {
  const auto frames = make_stream(20, 2);
  KeyframeIndex index(kStreamTimeBase, /*max_segments=*/3);
  REQUIRE(index.build(frames));
  CHECK(index.entries().size() == 3);
  CHECK(index.truncated());
  const SeekResult hit = index.seek(0);
  CHECK(hit.found);
}

TEST_CASE("empty input builds no index", "[index]") {
  KeyframeIndex index(kStreamTimeBase);
  CHECK_FALSE(index.build({}));
  CHECK(index.entries().empty());
}

TEST_CASE("microseconds convert with rounding", "[index]") {
  CHECK(microseconds_to_ticks(1000000, Rational{1, 1000}) == 1000);
  CHECK(microseconds_to_ticks(0, kStreamTimeBase) == 0);
  // 33333 us at 90 kHz is 2999.97 ticks -> 3000.
  CHECK(microseconds_to_ticks(33333, kStreamTimeBase) == 3000);
  CHECK(microseconds_to_ticks(500, Rational{1, 1000}) == 1);
}
