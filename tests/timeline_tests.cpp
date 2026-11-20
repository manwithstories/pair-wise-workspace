#include "media/timeline.hpp"

#include <catch2/catch_test_macros.hpp>

#include <cstdint>
#include <vector>

using namespace mediaqc;

namespace {

TimelineConfig video_config(Rational rate, ClockSpec clock) {
  TimelineConfig config;
  config.rate = rate;
  config.clock = clock;
  return config;
}

}  // namespace

TEST_CASE("rate to interval is exact for common broadcast rates", "[timeline]") {
  CHECK(TrackTimeline::compute_interval_ns(Rational{25, 1}) == 40000000);
  CHECK(TrackTimeline::compute_interval_ns(Rational{30, 1}) == 33333333);
  CHECK(TrackTimeline::compute_interval_ns(Rational{50, 1}) == 20000000);
  // 30000/1001 = 29.97002997 fps -> 33366667 ns (rounded).
  CHECK(TrackTimeline::compute_interval_ns(Rational{30000, 1001}) == 33366667);
  CHECK(TrackTimeline::compute_interval_ns(Rational{0, 1}) == 0);
}

TEST_CASE("regular cadence maps to an exact grid without repairs", "[timeline]") {
  // 25 fps, a 90 kHz clock (MPEG style) => 3600 ticks per frame.
  auto timeline = TrackTimeline(video_config(Rational{25, 1}, ClockSpec{90000, 0}));
  for (int i = 0; i < 100; ++i) {
    auto m = timeline.map(true, static_cast<std::int64_t>(i) * 3600);
    CHECK(m.ns == static_cast<Ns>(i) * 40000000);
    CHECK(m.reasons == 0);
  }
  CHECK(timeline.repaired() == 0);
  CHECK(timeline.frames() == 100);
}

TEST_CASE("32-bit counter wraparound is unwrapped onto one monotonic grid",
          "[timeline]") {
  // 48 kHz audio clock, 32-bit counter, 20 ms frames => 960 ticks.
  ClockSpec clock{48000, 32};
  auto timeline = TrackTimeline(video_config(Rational{50, 1}, clock));
  const std::uint64_t modulus = 1ULL << 32;
  const std::int64_t ticks_per_frame = 960;
  // Start near the top of the range so the counter wraps mid-run.
  const std::uint64_t start = modulus - ticks_per_frame * 3;
  bool saw_wrap = false;
  for (int i = 0; i < 20; ++i) {
    std::uint64_t raw = (start + static_cast<std::uint64_t>(i) * ticks_per_frame) % modulus;
    auto m = timeline.map(true, static_cast<std::int64_t>(raw));
    CHECK(m.ns == static_cast<Ns>(i) * 20000000);
    if (has_reason(m.reasons, RepairReason::kWraparound)) {
      saw_wrap = true;
      CHECK(i == 3);
    }
  }
  CHECK(saw_wrap);
  CHECK(timeline.repaired() == 1);
}

TEST_CASE("backwards and repeated capture times are forced forward",
          "[timeline]") {
  auto timeline = TrackTimeline(video_config(Rational{25, 1}, ClockSpec{1000000000, 0}));
  std::vector<std::int64_t> ticks = {
      0, 40000000, 80000000, 40000000, 80000000, 80000000, 160000000,
  };
  Ns previous = -1;
  for (std::size_t i = 0; i < ticks.size(); ++i) {
    auto m = timeline.map(true, ticks[i]);
    // Output must be strictly increasing no matter what the device claims.
    CHECK(m.ns > previous);
    CHECK(m.ns >= static_cast<Ns>(i) * 40000000);
    // The two frames whose device value actually moved backwards or stood
    // still must carry the non-monotonic reason.
    if (i == 3 || i == 5) {
      CHECK(has_reason(m.reasons, RepairReason::kNonMonotonic));
    }
    previous = m.ns;
  }
  CHECK(timeline.repaired() >= 2);
}

TEST_CASE("missing capture times are derived from the frame rate", "[timeline]") {
  auto timeline = TrackTimeline(video_config(Rational{25, 1}, ClockSpec{1000000000, 0}));
  auto first = timeline.map(true, 0);
  CHECK(first.ns == 0);
  auto missing1 = timeline.map(false, 0);
  CHECK(missing1.ns == 40000000);
  CHECK(has_reason(missing1.reasons, RepairReason::kMissing));
  auto missing2 = timeline.map(false, 0);
  CHECK(missing2.ns == 80000000);
  auto resume = timeline.map(true, 160000000);
  CHECK(resume.ns == 120000000);
  CHECK(timeline.repaired() == 2);
}

TEST_CASE("drift against the declared rate is corrected to the grid",
          "[timeline]") {
  // The device clock runs 10% fast: it claims 44 ms between 40 ms frames.
  auto timeline = TrackTimeline(video_config(Rational{25, 1}, ClockSpec{1000000000, 0}));
  std::int64_t device = 0;
  std::uint64_t drift_frames = 0;
  for (int i = 0; i < 50; ++i) {
    auto m = timeline.map(true, device);
    // Output must stay on the 40 ms grid regardless of the fast clock.
    CHECK(m.ns == static_cast<Ns>(i) * 40000000);
    if (has_reason(m.reasons, RepairReason::kDrift)) {
      ++drift_frames;
    }
    device += 44000000;
  }
  CHECK(drift_frames > 0);
}

TEST_CASE("dropped frames advance the grid by the measured gap", "[timeline]") {
  auto timeline = TrackTimeline(video_config(Rational{25, 1}, ClockSpec{1000000000, 0}));
  auto a = timeline.map(true, 0);
  CHECK(a.ns == 0);
  // Next frame arrives 3 frame intervals later: two frames were dropped.
  auto b = timeline.map(true, 120000000);
  CHECK(b.ns == 120000000);
  auto c = timeline.map(true, 160000000);
  CHECK(c.ns == 160000000);
}

TEST_CASE("state restore yields identical continuation", "[timeline]") {
  auto config = video_config(Rational{25, 1}, ClockSpec{90000, 0});
  std::vector<std::int64_t> ticks;
  for (int i = 0; i < 40; ++i) {
    ticks.push_back(static_cast<std::int64_t>(i) * 3600);
  }
  // Corrupt a few ticks so repairs are exercised too.
  ticks[10] = 10 * 3600 - 5000;
  ticks[20] = ticks[19];

  auto straight = TrackTimeline(config);
  std::vector<Ns> direct;
  for (auto t : ticks) direct.push_back(straight.map(true, t).ns);

  auto part_a = TrackTimeline(config);
  for (int i = 0; i < 17; ++i) part_a.map(true, ticks[static_cast<std::size_t>(i)]);
  TimelineState saved = part_a.state();

  auto part_b = TrackTimeline(config);
  part_b.restore(saved);
  std::vector<Ns> resumed;
  for (std::size_t i = 17; i < ticks.size(); ++i) resumed.push_back(part_b.map(true, ticks[i]).ns);

  for (std::size_t i = 0; i < resumed.size(); ++i) {
    CHECK(resumed[i] == direct[i + 17]);
  }
}

TEST_CASE("ticks to nanoseconds rounds deterministically", "[timeline]") {
  Ns out = 0;
  REQUIRE(ticks_to_ns_checked(90000, 90000, out));
  CHECK(out == 1000000000);
  REQUIRE(ticks_to_ns_checked(1, 3, out));
  CHECK(out == 333333333);
  REQUIRE(ticks_to_ns_checked(2, 3, out));
  CHECK(out == 666666667);
  CHECK_FALSE(ticks_to_ns_checked(1, 0, out));
}
