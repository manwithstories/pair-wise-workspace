#pragma once

#include <cstddef>
#include <functional>
#include <string>
#include <vector>

#include "mediacore/error.hpp"

namespace mediacore {

// Pipeline stages every track flows through. The dependency order is:
// manifest parse -> probe -> audio planning -> metadata -> output verify.
enum class Stage {
    Parse = 0,
    Probe = 1,
    AudioPlan = 2,
    Metadata = 3,
    OutputVerify = 4,
};

const char* toString(Stage stage);

struct TaskNode {
    std::string track;
    Stage stage = Stage::Parse;
    std::string id() const { return track + "." + toString(stage); }
};

struct TaskResult {
    std::string nodeId;
    bool ok = true;
};

// Directed dependency graph over (track, stage) tasks. Edges express both
// the per-track stage chain and cross-track ordering.
class TrackGraph {
public:
    explicit TrackGraph(std::size_t expectedNodes = 0) {
        nodes_.reserve(expectedNodes);
    }

    // Adds a node; returns its index. Duplicate ids return the existing one.
    std::size_t addNode(TaskNode node);
    std::size_t size() const { return nodes_.size(); }
    const TaskNode& node(std::size_t i) const { return nodes_[i]; }
    std::size_t nodeIndex(const std::string& id) const;

    // Edge from -> to. Unknown endpoints are fatal usage errors signalled
    // via return value rather than exceptions.
    bool addEdge(const std::string& from, const std::string& to);
    const std::vector<std::size_t>& successors(std::size_t i) const;

    // Kahn topological sort with deterministic tie-breaking (node id
    // order). Returns nullopt and logs DependencyCycle on a cycle.
    std::optional<std::vector<std::size_t>> topologicalOrder(
        ErrorLog& errors) const;

    // Convenience: build the standard five-stage chain for one track plus
    // a cross-track "this track waits for other's parse" edge.
    void addTrackChain(const std::string& track);

private:
    std::vector<TaskNode> nodes_;
    std::vector<std::vector<std::size_t>> edges_;
    std::vector<std::size_t> empty_;
};

// Build the standard graph: each track gets its five-stage chain, and
// every non-first track's Probe waits on the first track's Parse so output
// ordering stays deterministic while independent stages still parallelize.
TrackGraph buildStandardGraph(const std::vector<std::string>& tracks,
                              ErrorLog& errors);

// Execute nodes in topological order with a bounded worker pool. The
// optional per-node action runs concurrently for independent nodes; the
// returned results vector is always in topological (deterministic) order.
std::vector<TaskResult> executeGraph(
    const TrackGraph& graph,
    const std::function<TaskResult(const TaskNode&)>& action,
    std::size_t maxWorkers, ErrorLog& errors);

}  // namespace mediacore
