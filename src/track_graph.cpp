#include "track_graph.hpp"

#include <algorithm>
#include <set>

namespace mb {

ThreadPool::ThreadPool(unsigned workers) {
  unsigned n = std::max(1u, workers);
  for (unsigned i = 0; i < n; ++i) {
    workers_.emplace_back([this] {
      for (;;) {
        std::function<void()> job;
        {
          std::unique_lock<std::mutex> lk(m_);
          cv_jobs_.wait(lk, [this] { return stop_ || !jobs_.empty(); });
          if (stop_ && jobs_.empty()) return;
          job = std::move(jobs_.front());
          jobs_.pop();
          ++active_;
        }
        job();
        {
          std::lock_guard<std::mutex> lk(m_);
          --active_;
          if (jobs_.empty() && active_ == 0) cv_done_.notify_all();
        }
      }
    });
  }
}

ThreadPool::~ThreadPool() {
  {
    std::lock_guard<std::mutex> lk(m_);
    stop_ = true;
  }
  cv_jobs_.notify_all();
  for (auto& t : workers_)
    if (t.joinable()) t.join();
}

void ThreadPool::enqueue(std::function<void()> job) {
  {
    std::lock_guard<std::mutex> lk(m_);
    jobs_.push(std::move(job));
  }
  cv_jobs_.notify_one();
}

void ThreadPool::wait_all() {
  std::unique_lock<std::mutex> lk(m_);
  cv_done_.wait(lk, [this] { return jobs_.empty() && active_ == 0; });
}

void TrackGraph::add_node(const TrackNode& n) { nodes_[n.track_id] = n; }

void TrackGraph::add_dependency(u32 track_id, u32 depends_on) {
  nodes_[track_id].depends_on.push_back(depends_on);
}

GraphSchedule TrackGraph::topological_sort() const {
  GraphSchedule sched;
  std::map<u32, u32> indegree;
  std::map<u32, std::vector<u32>> dependents;
  std::set<u32> ready;
  for (const auto& [id, node] : nodes_) {
    (void)node;
    indegree[id] = 0;
    dependents[id] = {};
  }
  for (const auto& [id, node] : nodes_) {
    for (u32 dep : node.depends_on) {
      if (!nodes_.contains(dep)) continue;  // unknown deps ignored defensively
      indegree[id]++;
      dependents[dep].push_back(id);
    }
  }
  for (const auto& [id, deg] : indegree)
    if (deg == 0) ready.insert(id);
  while (!ready.empty()) {
    u32 id = *ready.begin();
    ready.erase(ready.begin());
    sched.order.push_back(id);
    for (u32 down : dependents[id]) {
      if (--indegree[down] == 0) ready.insert(down);
    }
  }
  if (sched.order.size() != nodes_.size()) {
    std::string stuck;
    for (const auto& [id, deg] : indegree)
      if (deg > 0) stuck += std::to_string(id) + " ";
    sched.cycle_detail = "cycle detected among tracks: " + stuck;
    sched.ok = false;
    return sched;
  }
  sched.ok = true;
  return sched;
}

GraphSchedule TrackGraph::run(
    const std::map<u32, std::function<bool()>>& tasks, unsigned max_workers) {
  GraphSchedule sched = topological_sort();
  if (!sched.ok) return sched;
  std::map<u32, bool> failed_set;
  std::map<u32, bool> skipped_set;
  // Execute level by level so failure state is visible before dependents run.
  std::map<u32, u32> remaining;
  for (const auto& [id, node] : nodes_)
    remaining[id] = static_cast<u32>(node.depends_on.size());
  std::set<u32> level;
  for (const auto& [id, deg] : remaining)
    if (deg == 0) level.insert(id);
  ThreadPool pool(max_workers);
  std::mutex state_m;
  while (!level.empty()) {
    std::vector<u32> current(level.begin(), level.end());
    level.clear();
    std::atomic<unsigned> failures{0};
    for (u32 id : current) {
      const TrackNode& node = nodes_.at(id);
      bool upstream_failed;
      {
        std::lock_guard<std::mutex> lk(state_m);
        upstream_failed = std::any_of(
            node.depends_on.begin(), node.depends_on.end(),
            [&](u32 d) { return failed_set[d] || skipped_set[d]; });
      }
      if (upstream_failed) {
        std::lock_guard<std::mutex> lk(state_m);
        skipped_set[id] = true;
        sched.skipped.push_back(id);
        continue;
      }
      auto it = tasks.find(id);
      pool.enqueue([&, id, it]() {
        bool ok = (it == tasks.end()) || it->second();
        if (!ok) {
          {
            std::lock_guard<std::mutex> lk(state_m);
            failed_set[id] = true;
          }
          failures.fetch_add(1, std::memory_order_relaxed);
        }
      });
    }
    pool.wait_all();
    (void)failures;
    for (u32 id : current) {
      if (failed_set[id]) sched.failed.push_back(id);
      // Unblock dependents of every node that finished this level.
      for (auto& [other, deg] : remaining) {
        const auto& deps = nodes_.at(other).depends_on;
        if (deg > 0 &&
            std::find(deps.begin(), deps.end(), id) != deps.end()) {
          --deg;
          if (deg == 0) level.insert(other);
        }
      }
    }
  }
  std::sort(sched.failed.begin(), sched.failed.end());
  std::sort(sched.skipped.begin(), sched.skipped.end());
  return sched;
}

}  // namespace mb
