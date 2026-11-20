#pragma once
// Track dependency graph: declares tracks and their A/V sync dependencies,
// rejects cycles, executes ready tasks on a bounded thread pool and marks
// downstream tasks skipped when an upstream task fails.
#include <atomic>
#include <condition_variable>
#include <functional>
#include <map>
#include <mutex>
#include <queue>
#include <string>
#include <thread>
#include <vector>

#include "common.hpp"

namespace mb {

struct TrackNode {
  u32 track_id = 0;
  std::string name;
  TrackKind kind = TrackKind::Video;
  std::vector<u32> depends_on;  // sync dependencies (e.g. audio waits on video)
  TaskStatus status = TaskStatus::Pending;
  std::string skip_reason;
  bool failed = false;
};

struct GraphSchedule {
  bool ok = false;             // false when a cycle was detected
  std::string cycle_detail;
  std::vector<u32> order;      // deterministic topological order
  std::vector<u32> skipped;    // tasks skipped due to upstream failure
  std::vector<u32> failed;
};

class ThreadPool {
 public:
  explicit ThreadPool(unsigned workers);
  ~ThreadPool();
  ThreadPool(const ThreadPool&) = delete;
  ThreadPool& operator=(const ThreadPool&) = delete;
  // Enqueue a unit of work; execution order among queued jobs is FIFO.
  void enqueue(std::function<void()> job);
  // Block until every enqueued job has completed.
  void wait_all();

 private:
  std::vector<std::thread> workers_;
  std::queue<std::function<void()>> jobs_;
  std::mutex m_;
  std::condition_variable cv_jobs_;
  std::condition_variable cv_done_;
  bool stop_ = false;
  unsigned active_ = 0;
};

class TrackGraph {
 public:
  void add_node(const TrackNode& n);
  void add_dependency(u32 track_id, u32 depends_on);

  // Kahn's algorithm; deterministic because ids are processed in ascending
  // order at every level.
  GraphSchedule topological_sort() const;

  // Run tasks in dependency order. A task returns false on failure; all of
  // its dependents are then marked skipped with a recorded reason. Worker
  // count is bounded so decode memory stays within the configured budget.
  GraphSchedule run(const std::map<u32, std::function<bool()>>& tasks,
                    unsigned max_workers);

  const std::map<u32, TrackNode>& nodes() const { return nodes_; }

 private:
  std::map<u32, TrackNode> nodes_;
};

}  // namespace mb
