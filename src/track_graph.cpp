#include <algorithm>
#include <queue>
#include <unordered_map>

#include "mediacore/track_graph.hpp"

namespace mediacore {

const char* toString(Stage stage) {
    switch (stage) {
        case Stage::Parse: return "parse";
        case Stage::Probe: return "probe";
        case Stage::AudioPlan: return "audio_plan";
        case Stage::Metadata: return "metadata";
        case Stage::OutputVerify: return "output_verify";
    }
    return "unknown";
}

std::size_t TrackGraph::addNode(TaskNode node) {
    const std::string id = node.id();
    auto found = nodeIndex(id);
    if (found != static_cast<std::size_t>(-1)) return found;
    nodes_.push_back(std::move(node));
    edges_.emplace_back();
    return nodes_.size() - 1;
}

std::size_t TrackGraph::nodeIndex(const std::string& id) const {
    for (std::size_t i = 0; i < nodes_.size(); ++i)
        if (nodes_[i].id() == id) return i;
    return static_cast<std::size_t>(-1);
}

bool TrackGraph::addEdge(const std::string& from,
                         const std::string& to) {
    const auto a = nodeIndex(from);
    const auto b = nodeIndex(to);
    if (a == static_cast<std::size_t>(-1) ||
        b == static_cast<std::size_t>(-1))
        return false;
    auto& succ = edges_[a];
    if (std::find(succ.begin(), succ.end(), b) == succ.end())
        succ.push_back(b);
    return true;
}

const std::vector<std::size_t>& TrackGraph::successors(
    std::size_t i) const {
    return i < edges_.size() ? edges_[i] : empty_;
}

void TrackGraph::addTrackChain(const std::string& track) {
    static const Stage chain[] = {Stage::Parse, Stage::Probe,
                                  Stage::AudioPlan, Stage::Metadata,
                                  Stage::OutputVerify};
    std::size_t prev = addNode({track, chain[0]});
    for (std::size_t i = 1; i < std::size(chain); ++i) {
        const std::size_t cur = addNode({track, chain[i]});
        edges_[prev].push_back(cur);
        prev = cur;
    }
    // Keep successor lists ordered by node id for determinism.
    for (auto& list : edges_)
        std::sort(list.begin(), list.end(), [&](std::size_t a,
                                                 std::size_t b) {
            return nodes_[a].id() < nodes_[b].id();
        });
}

std::optional<std::vector<std::size_t>> TrackGraph::topologicalOrder(
    ErrorLog& errors) const {
    std::vector<std::size_t> indegree(nodes_.size(), 0);
    for (const auto& list : edges_)
        for (std::size_t to : list) ++indegree[to];

    // Deterministic min-heap on the stable node id string.
    auto cmp = [&](std::size_t a, std::size_t b) {
        return nodes_[a].id() > nodes_[b].id();
    };
    std::priority_queue<std::size_t, std::vector<std::size_t>,
                        decltype(cmp)>
        ready(cmp);
    for (std::size_t i = 0; i < indegree.size(); ++i)
        if (indegree[i] == 0) ready.push(i);

    std::vector<std::size_t> order;
    order.reserve(nodes_.size());
    while (!ready.empty()) {
        const std::size_t cur = ready.top();
        ready.pop();
        order.push_back(cur);
        for (std::size_t to : edges_[cur])
            if (--indegree[to] == 0) ready.push(to);
    }
    if (order.size() != nodes_.size()) {
        errors.add({ErrorCode::DependencyCycle, "", -1, "",
                    {0, 0, {0, 1}}, false,
                    "track dependency graph contains a cycle"});
        return std::nullopt;
    }
    return order;
}

TrackGraph buildStandardGraph(const std::vector<std::string>& tracks,
                              ErrorLog& errors) {
    TrackGraph graph(tracks.size() * 5);
    for (const auto& t : tracks) graph.addTrackChain(t);
    // Cross-track ordering: later tracks' Probe waits for the first
    // track's Parse, keeping summaries reproducible without serializing
    // the independent audio/metadata stages.
    if (tracks.size() > 1) {
        const std::string gate =
            tracks.front() + "." + toString(Stage::Parse);
        for (std::size_t i = 1; i < tracks.size(); ++i) {
            const std::string probe =
                tracks[i] + "." + toString(Stage::Probe);
            if (!graph.addEdge(gate, probe))
                errors.add({ErrorCode::InvalidArgument, tracks[i], -1, "",
                            {0, 0, {0, 1}}, false,
                            "failed to wire cross-track dependency"});
        }
    }
    return graph;
}

}  // namespace mediacore
