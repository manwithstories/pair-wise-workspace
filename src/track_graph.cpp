#include "media/track_graph.hpp"

#include <algorithm>
#include <functional>
#include <map>
#include <queue>
#include <set>
#include <string>
#include <vector>

namespace mediaqc {
namespace {

OrderError make_error(ErrorCategory category, std::string code,
                      std::string message, std::string track) {
  OrderError error;
  error.error.category = category;
  error.error.code = std::move(code);
  error.error.message = std::move(message);
  error.track = std::move(track);
  error.error.track = error.track;
  return error;
}

// Depth-first walk restricted to unresolved nodes; fills `cycle` with the
// ids on the first cycle found (closing node repeated once).
bool find_cycle(const std::vector<TrackDesc>& tracks,
                const std::map<std::string, std::size_t, std::less<>>& index,
                const std::vector<bool>& resolved,
                std::vector<std::string>& cycle) {
  enum class Mark { kWhite, kGray, kBlack };
  std::vector<Mark> mark(tracks.size(), Mark::kWhite);
  std::vector<std::size_t> stack;

  std::function<bool(std::size_t)> visit = [&](std::size_t i) -> bool {
    mark[i] = Mark::kGray;
    stack.push_back(i);
    for (const std::string& dep : tracks[i].depends_on) {
      auto it = index.find(dep);
      if (it == index.end()) continue;
      std::size_t j = it->second;
      if (resolved[j]) continue;
      if (mark[j] == Mark::kGray) {
        auto begin = std::find(stack.begin(), stack.end(), j);
        cycle.clear();
        for (auto s = begin; s != stack.end(); ++s) {
          cycle.push_back(tracks[*s].id);
        }
        cycle.push_back(tracks[j].id);
        return true;
      }
      if (mark[j] == Mark::kWhite && visit(j)) {
        return true;
      }
    }
    mark[i] = Mark::kBlack;
    stack.pop_back();
    return false;
  };

  for (std::size_t i = 0; i < tracks.size(); ++i) {
    if (!resolved[i] && mark[i] == Mark::kWhite && visit(i)) {
      return true;
    }
  }
  return false;
}

}  // namespace

bool TrackGraph::build(const std::vector<TrackDesc>& tracks, TrackOrder& out,
                       OrderError& error) {
  out = TrackOrder{};
  const std::size_t n = tracks.size();
  if (n == 0) {
    error = make_error(ErrorCategory::kInvalidInput, "no_tracks",
                       "delivery declares no tracks", {});
    return false;
  }

  // Index by id, rejecting duplicates with the first repeated occurrence.
  std::map<std::string, std::size_t, std::less<>> index;
  for (std::size_t i = 0; i < n; ++i) {
    const std::string& id = tracks[i].id;
    if (id.empty()) {
      error = make_error(ErrorCategory::kInvalidInput, "empty_track_id",
                         "track " + std::to_string(i) + " has an empty id", id);
      return false;
    }
    auto [it, inserted] = index.emplace(id, i);
    if (!inserted) {
      error = make_error(ErrorCategory::kInvalidInput, "duplicate_track",
                         "duplicate track id '" + id + "' (first at index " +
                             std::to_string(it->second) + ")",
                         id);
      return false;
    }
  }

  // Build edges dependency -> dependant and reject unknown ids.
  std::vector<std::vector<std::size_t>> dependants(n);
  std::vector<std::size_t> indegree(n, 0);
  for (std::size_t i = 0; i < n; ++i) {
    std::set<std::string> seen;
    for (const std::string& dep : tracks[i].depends_on) {
      if (dep == tracks[i].id) {
        error = make_error(ErrorCategory::kInvalidInput, "self_dependency",
                           "track '" + tracks[i].id + "' depends on itself",
                           tracks[i].id);
        error.cycle = {dep, dep};
        return false;
      }
      if (!seen.insert(dep).second) {
        error = make_error(ErrorCategory::kInvalidInput, "duplicate_dependency",
                           "track '" + tracks[i].id + "' lists dependency '" +
                               dep + "' more than once",
                           tracks[i].id);
        return false;
      }
      auto it = index.find(dep);
      if (it == index.end()) {
        error = make_error(ErrorCategory::kInvalidInput, "missing_dependency",
                           "track '" + tracks[i].id +
                               "' depends on unknown track '" + dep + "'",
                           tracks[i].id);
        return false;
      }
      dependants[it->second].push_back(i);
      ++indegree[i];
    }
  }

  // Kahn's algorithm; the min-heap on declaration index keeps the order
  // stable across runs.
  std::priority_queue<std::size_t, std::vector<std::size_t>, std::greater<>>
      ready;
  for (std::size_t i = 0; i < n; ++i) {
    if (indegree[i] == 0) ready.push(i);
  }
  out.order.reserve(n);
  while (!ready.empty()) {
    std::size_t i = ready.top();
    ready.pop();
    out.order.push_back(i);
    for (std::size_t next : dependants[i]) {
      if (--indegree[next] == 0) ready.push(next);
    }
  }

  if (out.order.size() != n) {
    std::vector<bool> resolved(n, false);
    for (std::size_t i : out.order) resolved[i] = true;
    std::string first;
    for (std::size_t i = 0; i < n; ++i) {
      if (!resolved[i]) {
        first = tracks[i].id;
        break;
      }
    }
    std::vector<std::string> cycle;
    find_cycle(tracks, index, resolved, cycle);
    error = make_error(ErrorCategory::kInvalidInput, "dependency_cycle",
                       "track dependency cycle detected starting at '" + first +
                           "'",
                       first);
    error.cycle = std::move(cycle);
    return false;
  }

  out.report.reserve(n);
  for (std::size_t i : out.order) out.report.push_back(tracks[i].id);
  return true;
}

}  // namespace mediaqc
