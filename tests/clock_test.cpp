#include <catch2/catch_test_macros.hpp>

#include <algorithm>
#include <vector>

#include "media/json.hpp"
#include "media/timeline.hpp"
#include "media/track_graph.hpp"

TEST_CASE("json roundtrip and strict parse errors") {
    using namespace mediaqc::json;
    Value root(Object{{"a", 1}, {"b", Value(Array{Value(2), Value("x"), Value(true)})}});
    auto parsed = parse(dump(root));
    REQUIRE(parsed.find("a")->as_int() == 1);
    REQUIRE(parsed.find("b")->as_array().size() == 3);
    REQUIRE_THROWS(parse("{bad"));
    REQUIRE_THROWS(parse("[1,2,]"));
}

TEST_CASE("clock mapping residual stays below one output period") {
    mediaqc::SampleClock out(48000);
    int64_t max_res = 0;
    for (int64_t i = 0; i < 100000; ++i) {
        out.map_samples(i, 44100);
        max_res = std::max(max_res, out.last_map_residual_ns());
    }
    REQUIRE(max_res < out.period_ns());  // < 20834 ns at 48 kHz
    REQUIRE(out.samples_to_ns(48000) == 1'000'000'000LL);
}

TEST_CASE("packet mapper bounds cumulative cross-rate error under 20834 ns") {
    mediaqc::TimelineMapper mapper(48000);
    const int frame = 1024;
    for (int64_t pts = 0; pts < 44100 * 5; pts += frame)
        mapper.map_packet(pts, 44100, frame);
    REQUIRE(mapper.max_rounding_ns() < 20834);
    REQUIRE(mapper.total_samples() >= 5 * 48000 - 1);
}

TEST_CASE("track graph detects cycles, missing deps and overlaps") {
    mediaqc::TrackGraph g;
    g.blocks.push_back({"a", "t1", "a.wav", {"b"}, {0, 100}, 0});
    g.blocks.push_back({"b", "t1", "b.wav", {"a"}, {100, 200}, 1});
    auto rep = mediaqc::validate_graph(g);
    REQUIRE_FALSE(rep.ok);
    REQUIRE(rep.cycle_path.find("a") != std::string::npos);
    REQUIRE(rep.errors.front().code == mediaqc::ErrorCode::Cycle);

    mediaqc::TrackGraph g2;
    g2.blocks.push_back({"a", "t1", "a.wav", {"ghost"}, {0, 100}, 0});
    auto rep2 = mediaqc::validate_graph(g2);
    REQUIRE_FALSE(rep2.ok);
    REQUIRE(rep2.errors.front().code == mediaqc::ErrorCode::MissingDependency);

    mediaqc::TrackGraph g3;
    g3.blocks.push_back({"a", "t1", "a.wav", {}, {0, 100}, 0});
    g3.blocks.push_back({"b", "t1", "b.wav", {"a"}, {50, 150}, 1});
    auto rep3 = mediaqc::validate_graph(g3);
    REQUIRE_FALSE(rep3.ok);
    REQUIRE(rep3.errors.front().code == mediaqc::ErrorCode::DuplicateSegment);

    mediaqc::TrackGraph g4;
    g4.blocks.push_back({"a", "t1", "a.wav", {}, {0, 100}, 0});
    g4.blocks.push_back({"b", "t1", "b.wav", {"a"}, {100, 200}, 1});
    auto rep4 = mediaqc::validate_graph(g4);
    REQUIRE(rep4.ok);
    REQUIRE(rep4.order == std::vector<std::string>{"a", "b"});
}
