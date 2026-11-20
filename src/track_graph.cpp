#include "media/track_graph.hpp"

#include <algorithm>
#include <functional>
#include <map>
#include <numeric>
#include <set>
#include <unordered_map>

namespace mediaqc {

const InputBlock* TrackGraph::find(const std::string& id) const {
    for (const auto& b : blocks)
        if (b.id == id) return &b;
    return nullptr;
}

GraphReport validate_graph(const TrackGraph& graph) {
    GraphReport report;
    std::unordered_map<std::string, const InputBlock*> by_id;
    by_id.reserve(graph.blocks.size());
    for (const auto& b : graph.blocks) {
        if (by_id.count(b.id)) {
            report.ok = false;
            report.errors.push_back(
                {ErrorCode::DuplicateSegment, -1, b.span.start_sample, b.span.end_sample,
                 "duplicate block id: " + b.id});
        }
        by_id[b.id] = &b;
    }

    // Unknown dependencies.
    for (const auto& b : graph.blocks) {
        for (const auto& dep : b.depends_on) {
            if (!by_id.count(dep)) {
                report.ok = false;
                report.errors.push_back(
                    {ErrorCode::MissingDependency, -1, b.span.start_sample, b.span.end_sample,
                     "block " + b.id + " depends on unknown block " + dep});
            }
        }
    }

    // Overlapping segments on the same track are duplicate coverage.
    std::map<std::string, std::vector<const InputBlock*>> per_track;
    for (const auto& b : graph.blocks) per_track[b.track].push_back(&b);
    for (const auto& [track, bs] : per_track) {
        (void)track;
        for (size_t i = 0; i < bs.size(); ++i)
            for (size_t j = i + 1; j < bs.size(); ++j) {
                if (bs[i]->span.overlaps(bs[j]->span)) {
                    report.ok = false;
                    report.errors.push_back(
                        {ErrorCode::DuplicateSegment, -1,
                         std::max(bs[i]->span.start_sample, bs[j]->span.start_sample),
                         std::min(bs[i]->span.end_sample, bs[j]->span.end_sample),
                         "overlapping segments " + bs[i]->id + " and " + bs[j]->id});
                }
            }
    }

    // Kahn's topological sort; adjacency dep -> dependent.
    std::unordered_map<std::string, std::vector<std::string>> adj;
    std::unordered_map<std::string, int> indegree;
    for (const auto& b : graph.blocks) indegree.emplace(b.id, 0);
    for (const auto& b : graph.blocks) {
        for (const auto& dep : b.depends_on) {
            if (!by_id.count(dep)) continue;  // already reported
            adj[dep].push_back(b.id);
            ++indegree[b.id];
        }
    }
    std::set<std::string> ready;
    for (const auto& [id, deg] : indegree) if (deg == 0) ready.insert(id);
    while (!ready.empty()) {
        const std::string id = *ready.begin();
        ready.erase(ready.begin());
        report.order.push_back(id);
        for (const auto& next : adj[id])
            if (--indegree[next] == 0) ready.insert(next);
    }
    if (static_cast<int>(report.order.size()) != static_cast<int>(indegree.size())) {
        report.ok = false;
        // Reconstruct one concrete cycle path via DFS over remaining nodes.
        std::unordered_map<std::string, int> color;  // 0 white, 1 gray, 2 black
        std::vector<std::string> stack;
        std::function<bool(const std::string&)> dfs = [&](const std::string& id) -> bool {
            color[id] = 1;
            stack.push_back(id);
            const auto* b = by_id[id];
            for (const auto& dep : b->depends_on) {
                if (!by_id.count(dep)) continue;
                if (indegree[dep] == 0) continue;  // node already drained by Kahn
                if (color[dep] == 0) {
                    if (dfs(dep)) return true;
                } else if (color[dep] == 1) {
                    auto it = std::find(stack.begin(), stack.end(), dep);
                    report.cycle_path = std::accumulate(
                        it, stack.end(), std::string(),
                        [](const std::string& acc, const std::string& n) {
                            return acc.empty() ? n : acc + " -> " + n;
                        }) + " -> " + dep;
                    return true;
                }
            }
            color[id] = 2;
            stack.pop_back();
            return false;
        };
        for (const auto& [id, deg] : indegree)
            if (deg > 0 && color[id] == 0 && dfs(id)) break;
        if (report.cycle_path.empty()) report.cycle_path = "unresolved cycle";
        report.errors.push_back({ErrorCode::Cycle, -1, 0, 0,
                                 "dependency cycle detected: " + report.cycle_path});
        report.order.clear();
    }
    return report;
}

}  // namespace mediaqc
