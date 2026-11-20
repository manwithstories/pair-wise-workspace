#include <atomic>
#include <chrono>
#include <set>
#include <thread>

#include <catch2/catch_test_macros.hpp>

#include "mediacore/track_graph.hpp"

using namespace mediacore;

TEST_CASE("standard graph wires five-stage chain per track", "[graph]") {
    ErrorLog errors;
    auto graph = buildStandardGraph({"interview", "bcam", "narration"},
                                    errors);
    REQUIRE(graph.size() == 15);
    REQUIRE_FALSE(errors.hasFatal());
    auto order = graph.topologicalOrder(errors);
    REQUIRE(order.has_value());
    REQUIRE(order->size() == 15);

    // Stage ordering within a track must always be respected.
    auto pos = [&](Stage s, const std::string& t) {
        const std::string id = t + "." + toString(s);
        for (std::size_t i = 0; i < order->size(); ++i)
            if (graph.node((*order)[i]).id() == id) return i;
        return static_cast<std::size_t>(-1);
    };
    for (const auto* t : {"interview", "bcam", "narration"}) {
        REQUIRE(pos(Stage::Parse, t) < pos(Stage::Probe, t));
        REQUIRE(pos(Stage::Probe, t) < pos(Stage::AudioPlan, t));
        REQUIRE(pos(Stage::AudioPlan, t) < pos(Stage::Metadata, t));
        REQUIRE(pos(Stage::Metadata, t) < pos(Stage::OutputVerify, t));
    }
}

TEST_CASE("topological order is deterministic across runs", "[graph]") {
    ErrorLog errors;
    auto graph = buildStandardGraph({"zeta", "alpha", "mid"}, errors);
    auto first = graph.topologicalOrder(errors);
    auto second = graph.topologicalOrder(errors);
    REQUIRE(first.has_value());
    REQUIRE(*first == *second);
    // Smallest ready id is picked first.
    REQUIRE(graph.node((*first)[0]).id() == "alpha.parse");
}

TEST_CASE("cyclic dependency is rejected with a fatal error", "[graph]") {
    ErrorLog errors;
    TrackGraph graph;
    graph.addNode({"a", Stage::Parse});
    graph.addNode({"b", Stage::Probe});
    graph.addNode({"c", Stage::Metadata});
    REQUIRE(graph.addEdge("a.parse", "b.probe"));
    REQUIRE(graph.addEdge("b.probe", "c.metadata"));
    REQUIRE(graph.addEdge("c.metadata", "a.parse"));  // closes cycle
    auto order = graph.topologicalOrder(errors);
    REQUIRE_FALSE(order.has_value());
    REQUIRE(errors.count(ErrorCode::DependencyCycle) == 1);
    REQUIRE_FALSE(errors.items().front().recoverable);
    REQUIRE_FALSE(graph.addEdge("missing.node", "a.parse"));
}

TEST_CASE("bounded concurrent execution keeps deterministic output",
          "[graph]") {
    ErrorLog errors;
    auto graph = buildStandardGraph({"t0", "t1", "t2", "t3"}, errors);
    std::atomic<int> concurrent{0};
    std::atomic<int> maxConcurrent{0};
    auto order = graph.topologicalOrder(errors);

    auto results = executeGraph(
        graph,
        [&](const TaskNode& n) {
            int now = ++concurrent;
            int prev = maxConcurrent.load();
            while (now > prev &&
                   !maxConcurrent.compare_exchange_weak(prev, now)) {}
            std::this_thread::sleep_for(std::chrono::milliseconds(2));
            --concurrent;
            return TaskResult{n.id(), true};
        },
        2, errors);

    REQUIRE(results.size() == graph.size());
    REQUIRE(maxConcurrent <= 2);
    // Results must come back in the same topological order.
    for (std::size_t i = 0; i < order->size(); ++i)
        REQUIRE(results[i].nodeId == graph.node((*order)[i]).id());
    std::set<std::string> unique;
    for (const auto& r : results) unique.insert(r.nodeId);
    REQUIRE(unique.size() == graph.size());
}
