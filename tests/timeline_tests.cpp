#include <catch2/catch_test_macros.hpp>

#include "timeline.hpp"

using namespace mediaqc;

namespace {

TimelineOptions opts(int64_t est = 1000) {
  TimelineOptions o;
  o.time_base = Rational{1, 1000};
  o.estimated_duration = est;
  o.retain_samples = true;
  return o;
}

}  // namespace

TEST_CASE("monotonic input passes through unchanged") {
  auto o = opts();
  TimelineResult r;
  r.time_base = o.time_base;
  for (int i = 0; i < 5; ++i) {
    REQUIRE(timeline_push(o, r, i * 1000, i * 1000, 1000, i * 500, 500, 0, nullptr));
  }
  REQUIRE(r.ok);
  REQUIRE(r.stats.packets == 5);
  REQUIRE(r.samples[4].dts == 4000);
  REQUIRE(r.repairs.empty());
  REQUIRE(r.first_out_dts == 0);
}

TEST_CASE("missing DTS derived from PTS") {
  auto o = opts();
  TimelineResult r;
  r.time_base = o.time_base;
  timeline_push(o, r, 0, 0, 1000, 0, 10, 0, nullptr);
  timeline_push(o, r, AV_NOPTS_VALUE, 1000, 1000, 10, 10, 0, nullptr);
  REQUIRE(r.ok);
  REQUIRE(r.stats.dts_missing == 1);
  REQUIRE(r.samples[1].dts == 1000);
  REQUIRE(r.repairs.size() == 1);
  REQUIRE(r.repairs[0].kind == RepairKind::DtsDerivedFromPts);
}

TEST_CASE("missing PTS derived from DTS") {
  auto o = opts();
  TimelineResult r;
  r.time_base = o.time_base;
  timeline_push(o, r, 0, AV_NOPTS_VALUE, 1000, 0, 10, 0, nullptr);
  REQUIRE(r.ok);
  REQUIRE(r.stats.pts_missing == 1);
  REQUIRE(r.samples[0].pts == 0);
}

TEST_CASE("32-bit rollover is unwrapped and stays monotonic") {
  auto o = opts(1000);
  o.wrap.period_ticks = 100000;
  o.wrap.threshold_ticks = 50000;
  TimelineResult r;
  r.time_base = o.time_base;
  // raw sequence: 97000, 98000, 99000, then wrapped to 500 ...
  timeline_push(o, r, 97000, 97000, 1000, 0, 10, 0, nullptr);
  timeline_push(o, r, 98000, 98000, 1000, 10, 10, 0, nullptr);
  timeline_push(o, r, 99000, 99000, 1000, 20, 10, 0, nullptr);
  REQUIRE(timeline_push(o, r, 500, 500, 1000, 30, 10, 0, nullptr));
  REQUIRE(r.ok);
  REQUIRE(r.stats.wrapped == 1);
  REQUIRE(r.samples.back().dts > 99000);
  REQUIRE(r.samples.back().dts == 100500);
  REQUIRE(r.repairs.back().kind == RepairKind::WrapUnwrapped);
}

TEST_CASE("small backwards DTS is clamped, not dropped") {
  auto o = opts(1000);
  TimelineResult r;
  r.time_base = o.time_base;
  timeline_push(o, r, 0, 0, 1000, 0, 10, 0, nullptr);
  timeline_push(o, r, 1000, 1000, 1000, 10, 10, 0, nullptr);
  timeline_push(o, r, 500, 1500, 1000, 20, 10, 0, nullptr);  // 500 ticks back
  REQUIRE(r.ok);
  REQUIRE(r.stats.backwards_shifted == 1);
  REQUIRE(r.samples[2].dts == 1000);
  // PTS keeps its relative offset (1500 - 500 = +1000 ahead of raw DTS).
  REQUIRE(r.samples[2].pts == 2000);
}

TEST_CASE("large ambiguous backwards jump fails the track") {
  auto o = opts(1000);
  o.wrap.period_ticks = 1000000;
  o.wrap.threshold_ticks = 500000;
  o.ambiguity_tolerance_ticks = 5000;
  TimelineResult r;
  r.time_base = o.time_base;
  timeline_push(o, r, 100000, 100000, 1000, 0, 10, 0, nullptr);
  timeline_push(o, r, 101000, 101000, 1000, 10, 10, 0, nullptr);
  REQUIRE_FALSE(timeline_push(o, r, 50000, 50000, 1000, 20, 10, 0, nullptr));
  REQUIRE_FALSE(r.ok);
  REQUIRE(r.stats.unrepairable == 1);
  REQUIRE(r.failure.find("backwards") != std::string::npos);
}

TEST_CASE("duplicate DTS stays monotonic and is reported") {
  auto o = opts(1000);
  TimelineResult r;
  r.time_base = o.time_base;
  timeline_push(o, r, 0, 0, 1000, 0, 10, 0, nullptr);
  timeline_push(o, r, 1000, 1000, 1000, 10, 10, 0, nullptr);
  REQUIRE(timeline_push(o, r, 1000, 2000, 1000, 20, 10, 0, nullptr));
  REQUIRE(r.ok);
  REQUIRE(r.stats.duplicates == 1);
  REQUIRE(r.samples.back().dts == 1000);
  REQUIRE(r.samples.back().pts == 2000);
}

TEST_CASE("start alignment preserves inter-track relative offset") {
  auto o = opts(1000);
  TimelineResult a, b;
  a.time_base = Rational{1, 1000};
  b.time_base = Rational{1, 1000};
  timeline_push(o, a, 0, 0, 1000, 0, 10, 0, nullptr);
  timeline_push(o, b, 500, 500, 1000, 0, 10, 0, nullptr);
  std::vector<TimelineResult*> tracks{&a, &b};
  timeline_align_starts(tracks);
  REQUIRE(timeline_first_canonical_pts(a) == 0);
  REQUIRE(timeline_first_canonical_pts(b) == 45000);  // 500 ms later
}

TEST_CASE("timeline to canonical uses integer math") {
  TimelineResult r;
  r.time_base = Rational{1, 10240};
  r.start_shift = 0;
  REQUIRE(timeline_to_canonical(r, 10240) == 90000);
  REQUIRE(timeline_to_canonical(r, AV_NOPTS_VALUE) == AV_NOPTS_VALUE);
}

TEST_CASE("gap detection counts large discontinuities") {
  auto o = opts(1000);
  o.gap_threshold_ticks = 5000;
  TimelineResult r;
  r.time_base = o.time_base;
  timeline_push(o, r, 0, 0, 1000, 0, 10, 0, nullptr);
  timeline_push(o, r, 1000, 1000, 1000, 10, 10, 0, nullptr);
  timeline_push(o, r, 20000, 20000, 1000, 20, 10, 0, nullptr);
  REQUIRE(r.gap_count == 1);
}
