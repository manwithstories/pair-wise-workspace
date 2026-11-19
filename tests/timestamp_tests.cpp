#include <catch2/catch_test_macros.hpp>

#include <cstdint>
#include <vector>

#include "media_types.hpp"
#include "gap_estimator.hpp"
#include "timestamp_repair.hpp"

using namespace mfs;

namespace {

constexpr int64_t kNominal = 33333;

RepairedFrame frame_at(uint64_t seq, int64_t micros) {
  RepairedFrame frame;
  frame.seq = seq;
  frame.capture_hz = 1000000;
  frame.raw_clock = static_cast<uint32_t>(micros);
  frame.ts_us = micros;
  return frame;
}

RepairedFrame missing_frame(uint64_t seq) {
  RepairedFrame frame;
  frame.seq = seq;
  frame.capture_hz = 1000000;
  frame.raw_clock = 0;
  frame.clock_missing = true;
  return frame;
}

TimestampOptions options() {
  TimestampOptions opts;
  opts.nominal_interval_us = kNominal;
  return opts;
}

bool strictly_increasing(const std::vector<RepairedFrame>& frames) {
  for (size_t i = 1; i < frames.size(); ++i) {
    if (frames[i].ts_us <= frames[i - 1].ts_us) {
      return false;
    }
  }
  return true;
}

}  // namespace

TEST_CASE("clean timestamps are preserved", "[timestamp]") {
  std::vector<RepairedFrame> frames;
  for (uint64_t i = 0; i < 10; ++i) {
    frames.push_back(frame_at(i, 1000 + static_cast<int64_t>(i) * kNominal));
  }
  TimestampRepair repair(options());
  const TimestampStats stats = repair.repair(frames);
  CHECK(stats.strictly_increasing);
  CHECK(stats.wrap_fixes == 0);
  CHECK(stats.missing_timestamps == 0);
  CHECK(stats.drift_fixes == 0);
  for (uint64_t i = 1; i < frames.size(); ++i) {
    CHECK(frames[i].ts_us - frames[i - 1].ts_us == kNominal);
    CHECK(frames[i].original_ts_us == frames[i].ts_us);
  }
}

TEST_CASE("32-bit clock wraparound is unwrapped", "[timestamp]") {
  std::vector<RepairedFrame> frames;
  // Start just before the 32-bit rollover, then continue past it.
  const int64_t start = 4294967295ll - 3 * kNominal;
  for (uint64_t i = 0; i < 10; ++i) {
    RepairedFrame frame;
    frame.seq = i;
    frame.capture_hz = 1000000;
    frame.raw_clock = static_cast<uint32_t>(start + static_cast<int64_t>(i) * kNominal);
    frame.ts_us = frame.raw_clock;
    frames.push_back(frame);
  }
  REQUIRE(frames[4].raw_clock < frames[3].raw_clock);

  TimestampRepair repair(options());
  const TimestampStats stats = repair.repair(frames);
  CHECK(stats.wrap_fixes >= 1);
  CHECK(strictly_increasing(frames));
  for (uint64_t i = 0; i < frames.size(); ++i) {
    CHECK(frames[i].original_ts_us == start + static_cast<int64_t>(i) * kNominal);
    CHECK(frames[i].ts_us == frames[i].original_ts_us);
  }
}

TEST_CASE("duplicate and backwards timestamps are pinned", "[timestamp]") {
  std::vector<RepairedFrame> frames;
  for (uint64_t i = 0; i < 6; ++i) {
    frames.push_back(frame_at(i, 1000 + static_cast<int64_t>(i) * kNominal));
  }
  frames[2].raw_clock = frames[1].raw_clock;                     // duplicate
  frames[4].raw_clock = static_cast<uint32_t>(frames[3].raw_clock - 2 * kNominal);  // backwards

  TimestampRepair repair(options());
  const TimestampStats stats = repair.repair(frames);
  CHECK(stats.duplicate_timestamps >= 1);
  CHECK(stats.backward_timestamps >= 1);
  CHECK(strictly_increasing(frames));
  CHECK(frames[2].ts_us > frames[1].ts_us);
  CHECK(frames[4].ts_us > frames[3].ts_us);
  // Original raw values stay inspectable.
  CHECK(frames[2].original_ts_us == 1000 + kNominal);
}

TEST_CASE("missing timestamps are interpolated between neighbours",
          "[timestamp]") {
  std::vector<RepairedFrame> frames;
  for (uint64_t i = 0; i < 6; ++i) {
    frames.push_back(frame_at(i, 1000 + static_cast<int64_t>(i) * kNominal));
  }
  frames[2] = missing_frame(2);
  frames[3] = missing_frame(3);

  TimestampRepair repair(options());
  const TimestampStats stats = repair.repair(frames);
  CHECK(stats.missing_timestamps == 2);
  CHECK(strictly_increasing(frames));
  CHECK(frames[2].original_ts_us == -1);
  CHECK(frames[2].ts_us == 1000 + 2 * kNominal);
  CHECK(frames[3].ts_us == 1000 + 3 * kNominal);
}

TEST_CASE("leading and trailing missing timestamps extend the cadence",
          "[timestamp]") {
  std::vector<RepairedFrame> frames{missing_frame(0), missing_frame(1)};
  for (uint64_t i = 2; i < 6; ++i) {
    frames.push_back(frame_at(i, 1000 + static_cast<int64_t>(i) * kNominal));
  }
  frames.push_back(missing_frame(6));

  TimestampRepair repair(options());
  const TimestampStats stats = repair.repair(frames);
  CHECK(stats.missing_timestamps == 3);
  CHECK(strictly_increasing(frames));
  CHECK(frames[0].ts_us == 1000);
  CHECK(frames[1].ts_us == 1000 + kNominal);
  CHECK(frames[6].ts_us == 1000 + 6 * kNominal);
}

TEST_CASE("small drift is snapped to the nominal cadence", "[timestamp]") {
  std::vector<RepairedFrame> frames;
  for (uint64_t i = 0; i < 8; ++i) {
    const int64_t jitter = (i % 2 == 0) ? 1500 : -1500;
    frames.push_back(frame_at(i, 1000 + static_cast<int64_t>(i) * kNominal + jitter));
  }
  TimestampRepair repair(options());
  const TimestampStats stats = repair.repair(frames);
  CHECK(stats.drift_fixes >= 1);
  CHECK(strictly_increasing(frames));
  CHECK(frames[7].ts_us - frames[1].ts_us == 6 * kNominal);
}

TEST_CASE("empty input is accepted", "[timestamp]") {
  std::vector<RepairedFrame> frames;
  TimestampRepair repair(options());
  const TimestampStats stats = repair.repair(frames);
  CHECK(stats.strictly_increasing);
  CHECK(frames.empty());
}

TEST_CASE("gap estimation interpolates between surrounding frames",
          "[timestamp]") {
  std::vector<RepairedFrame> frames;
  for (uint64_t seq : {0ull, 1ull, 5ull, 6ull}) {
    frames.push_back(frame_at(seq, 1000 + static_cast<int64_t>(seq) * kNominal));
  }
  std::vector<SequenceGap> gaps{{2, 4}};
  std::vector<LostRange> lost;
  GapEstimateOptions gap_options;
  gap_options.nominal_interval_us = kNominal;
  estimate_lost_ranges(gaps, frames, gap_options, lost);
  REQUIRE(lost.size() == 1);
  CHECK(lost[0].first_seq == 2);
  CHECK(lost[0].last_seq == 4);
  CHECK(lost[0].estimated_frames == 3);
  CHECK(lost[0].estimated_start_us == frames[0].ts_us + 2 * kNominal);
  CHECK(lost[0].estimated_end_us == frames[0].ts_us + 5 * kNominal);
}
