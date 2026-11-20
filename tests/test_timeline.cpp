#include <catch2/catch_test_macros.hpp>

#include "mediacore/timeline.hpp"

using namespace mediacore;

namespace {

SegmentRef seg(int64_t seq, int64_t num, int64_t den,
               std::optional<Tick> start = std::nullopt,
               Rational base = Rational{1, 1}) {
    SegmentRef s;
    s.sequence = seq;
    s.duration = Rational{num, den};
    s.startTick = start;
    s.tickBase = base;
    s.uri = "seg" + std::to_string(seq);
    return s;
}

Manifest manifestWith(const std::string& id,
                      std::vector<SegmentRef> segs,
                      bool ended = true) {
    Manifest m;
    m.format = ManifestFormat::Hls;
    MediaPlaylist pl;
    pl.name = id;
    pl.trackId = id;
    pl.ended = ended;
    Representation rep;
    rep.id = id;
    rep.segments = std::move(segs);
    pl.representations.push_back(std::move(rep));
    m.playlists.push_back(std::move(pl));
    return m;
}

}  // namespace

TEST_CASE("HLS segments place contiguously on microsecond base",
          "[timeline]") {
    auto m = manifestWith("v0", {seg(0, 1, 1), seg(1, 1, 1),
                                 seg(2, 1, 1)});
    ErrorLog errors;
    auto tracks = buildTimelines(m, TimelineOptions{}, errors);
    REQUIRE(tracks.size() == 1);
    const auto& t = tracks[0];
    REQUIRE(t.segments.size() == 3);
    REQUIRE(t.segments[0].range.start == 0);
    REQUIRE(t.segments[1].range.start == 1'000'000);
    REQUIRE(t.cursor == 3'000'000);
    REQUIRE_FALSE(t.incomplete);
    REQUIRE_FALSE(errors.hasFatal());
}

TEST_CASE("non-divisible duration is refused, never rounded",
          "[timeline]") {
    // 1/3 s cannot land exactly on a 1/1,000,000 scale.
    auto m = manifestWith("v0", {seg(0, 1, 3)});
    ErrorLog errors;
    auto tracks = buildTimelines(m, TimelineOptions{}, errors);
    REQUIRE(errors.count(ErrorCode::NonDivisibleScale) == 1);
    REQUIRE(tracks[0].incomplete);
    REQUIRE(tracks[0].segments.empty());
}

TEST_CASE("explicit DASH ticks detect backward, duplicate and gap",
          "[timeline]") {
    const Rational base{1, 1000};
    auto m = manifestWith(
        "a0",
        {seg(0, 1000, 1000, 0, base), seg(1, 1000, 1000, 1000, base),
         seg(2, 1000, 1000, 2500, base),  // gap [2000,2500)
         seg(3, 1000, 1000, 2500, base)});  // duplicate start
    ErrorLog errors;
    auto tracks = buildTimelines(m, TimelineOptions{}, errors);
    REQUIRE(errors.count(ErrorCode::TimelineGap) == 1);
    REQUIRE(errors.count(ErrorCode::DuplicateSegment) == 1);

    auto m2 = manifestWith(
        "a1", {seg(0, 1000, 1000, 1000, base),
               seg(1, 1000, 1000, 500, base)});  // backward
    ErrorLog e2;
    auto t2 = buildTimelines(m2, TimelineOptions{}, e2);
    REQUIRE(e2.count(ErrorCode::TimestampBackward) == 1);
    REQUIRE(t2[0].incomplete);
}

TEST_CASE("zero-length segments are located errors", "[timeline]") {
    auto m = manifestWith("v0", {seg(0, 0, 1), seg(1, 1, 1)});
    ErrorLog errors;
    auto tracks = buildTimelines(m, TimelineOptions{}, errors);
    REQUIRE(errors.count(ErrorCode::ZeroLengthRange) == 1);
    const auto& e = errors.items().front();
    REQUIRE(e.track == "v0");
    REQUIRE(e.segment == 0);
    REQUIRE(e.range.length() == 0);
}

TEST_CASE("cross-track overlaps align on a common exact base",
          "[timeline]") {
    const Rational ms{1, 1000};
    auto m = manifestWith("interview",
                          {seg(0, 2000, 1000, 0, ms),
                           seg(1, 2000, 1000, 2000, ms)});
    m.playlists.push_back(
        manifestWith("narration", {seg(0, 3000, 1000, 1000, ms)})
            .playlists[0]);
    ErrorLog errors;
    auto tracks = buildTimelines(m, TimelineOptions{}, errors);
    auto common = unifiedTimebase(tracks);
    REQUIRE(common.has_value());
    auto overlaps = findCrossTrackOverlaps(tracks);
    REQUIRE(overlaps.size() == 2);
    REQUIRE(overlaps[0].trackA == "interview");
    REQUIRE(overlaps[0].trackB == "narration");
}

TEST_CASE("open-ended playlist marks the track incomplete", "[timeline]") {
    auto m = manifestWith("v0", {seg(0, 1, 1)}, /*ended=*/false);
    ErrorLog errors;
    auto tracks = buildTimelines(m, TimelineOptions{}, errors);
    REQUIRE(tracks[0].incomplete);
}
