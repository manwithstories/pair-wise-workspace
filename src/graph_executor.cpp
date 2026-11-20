#include <atomic>
#include <condition_variable>
#include <mutex>
#include <queue>
#include <thread>

#include "mediacore/track_graph.hpp"

namespace mediacore {

namespace {

// Bounded thread pool: at most maxWorkers tasks run at once and the
// pending queue is fed by topological readiness, so memory stays bounded
// regardless of manifest size.
class BoundedPool {
public:
    explicit BoundedPool(std::size_t maxWorkers) {
        const std::size_t hw = std::thread::hardware_concurrency();
        std::size_t cap = maxWorkers == 0 ? 1 : maxWorkers;
        if (hw > 0) cap = std::min(cap, hw);
        workers_.reserve(cap);
        for (std::size_t i = 0; i < cap; ++i)
            workers_.emplace_back([this] { workerLoop(); });
    }

    ~BoundedPool() {
        {
            std::lock_guard<std::mutex> lock(mu_);
            stop_ = true;
        }
        cv_.notify_all();
        for (auto& t : workers_)
            if (t.joinable()) t.join();
    }

    void submit(std::function<void()> job) {
        {
            std::lock_guard<std::mutex> lock(mu_);
            jobs_.push(std::move(job));
        }
        cv_.notify_one();
    }

    void waitDrained() {
        std::unique_lock<std::mutex> lock(mu_);
        done_.wait(lock, [this] {
            return jobs_.empty() && active_ == 0;
        });
    }

private:
    void workerLoop() {
        for (;;) {
            std::function<void()> job;
            {
                std::unique_lock<std::mutex> lock(mu_);
                cv_.wait(lock, [this] { return stop_ || !jobs_.empty(); });
                if (stop_ && jobs_.empty()) return;
                job = std::move(jobs_.front());
                jobs_.pop();
                ++active_;
            }
            job();
            {
                std::lock_guard<std::mutex> lock(mu_);
                --active_;
                if (jobs_.empty() && active_ == 0) done_.notify_all();
            }
        }
    }

    std::vector<std::thread> workers_;
    std::queue<std::function<void()>> jobs_;
    std::mutex mu_;
    std::condition_variable cv_;
    std::condition_variable done_;
    std::size_t active_ = 0;
    bool stop_ = false;
};

}  // namespace

std::vector<TaskResult> executeGraph(
    const TrackGraph& graph,
    const std::function<TaskResult(const TaskNode&)>& action,
    std::size_t maxWorkers, ErrorLog& errors) {
    auto order = graph.topologicalOrder(errors);
    std::vector<TaskResult> results;
    if (!order) return results;

    std::vector<std::size_t> indegree(graph.size(), 0);
    for (std::size_t i = 0; i < graph.size(); ++i)
        for (std::size_t to : graph.successors(i)) ++indegree[to];

    results.resize(graph.size());
    std::vector<char> finished(graph.size(), false);
    std::mutex stateMu;
    BoundedPool pool(maxWorkers == 0 ? 1 : maxWorkers);

    std::function<void(std::size_t)> scheduleReady =
        [&](std::size_t nodeIdx) {
            pool.submit([&, nodeIdx] {
                TaskResult r = action(graph.node(nodeIdx));
                r.nodeId = graph.node(nodeIdx).id();
                {
                    std::lock_guard<std::mutex> lock(stateMu);
                    results[nodeIdx] = std::move(r);
                    finished[nodeIdx] = true;
                }
                for (std::size_t to : graph.successors(nodeIdx)) {
                    bool fire = false;
                    {
                        std::lock_guard<std::mutex> lock(stateMu);
                        if (--indegree[to] == 0) fire = true;
                    }
                    if (fire) scheduleReady(to);
                }
            });
        };

    for (std::size_t i = 0; i < graph.size(); ++i)
        if (indegree[i] == 0) scheduleReady(i);
    pool.waitDrained();

    // Re-emit in topological order for byte-identical downstream output.
    std::vector<TaskResult> ordered;
    ordered.reserve(results.size());
    for (std::size_t idx : *order)
        if (finished[idx]) ordered.push_back(std::move(results[idx]));
    return ordered;
}

}  // namespace mediacore
