#pragma once

// Bounded pipeline primitives: a byte-bounded blocking queue that propagates
// backpressure upstream, a cancellation token that releases blocked stages,
// a memory tracker enforcing the task memory ceiling, and a bandwidth
// estimator that continuously re-estimates the ingest pace.

#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <mutex>
#include <optional>
#include <utility>

namespace relay {

class CancelToken {
 public:
  void cancel() { cancelled_.store(true, std::memory_order_relaxed); }
  bool cancelled() const { return cancelled_.load(std::memory_order_relaxed); }

 private:
  std::atomic<bool> cancelled_{false};
};

// Accounts queued payload bytes against a fixed ceiling and records the peak.
class MemoryTracker {
 public:
  explicit MemoryTracker(int64_t limit_bytes) : limit_(limit_bytes) {}
  bool try_acquire(int64_t bytes) {
    int64_t cur = current_.load(std::memory_order_relaxed);
    while (true) {
      if (cur + bytes > limit_) return false;
      if (current_.compare_exchange_weak(cur, cur + bytes)) break;
    }
    int64_t now = cur + bytes;
    int64_t p = peak_.load(std::memory_order_relaxed);
    while (now > p && !peak_.compare_exchange_weak(p, now)) {
    }
    return true;
  }
  void release(int64_t bytes) { current_.fetch_sub(bytes, std::memory_order_relaxed); }
  int64_t peak() const { return peak_.load(std::memory_order_relaxed); }
  int64_t limit() const { return limit_; }

 private:
  int64_t limit_;
  std::atomic<int64_t> current_{0};
  std::atomic<int64_t> peak_{0};
};

// Blocking bounded queue. push() blocks while full (item count or memory
// ceiling) — that stall is the backpressure signal to the upstream stage.
// Cancellation or close() wakes every blocked party and drains ownership.
template <typename T>
class BoundedQueue {
 public:
  BoundedQueue(size_t max_items, MemoryTracker& mem)
      : max_items_(max_items == 0 ? 1 : max_items), mem_(mem) {}

  // Returns false when cancelled or closed before the item could be queued.
  bool push(T item, int64_t bytes, CancelToken& ct) {
    std::unique_lock<std::mutex> lk(m_);
    bool acquired = false;  // bytes reserved for this item, rolled back on abort
    not_full_.wait(lk, [&] {
      if (closed_ || ct.cancelled()) return true;
      if (q_.size() >= max_items_) return false;
      if (!acquired) acquired = mem_.try_acquire(bytes);
      return acquired;
    });
    if (closed_ || ct.cancelled()) {
      if (acquired) mem_.release(bytes);
      return false;
    }
    q_.emplace_back(std::move(item), bytes);
    not_empty_.notify_one();
    return true;
  }

  std::optional<T> pop(CancelToken& ct) {
    std::unique_lock<std::mutex> lk(m_);
    not_empty_.wait(lk, [&] { return closed_ || ct.cancelled() || !q_.empty(); });
    if (q_.empty()) return std::nullopt;
    auto [item, bytes] = std::move(q_.front());
    q_.pop_front();
    lk.unlock();
    mem_.release(bytes);
    not_full_.notify_one();
    return item;
  }

  void close() {
    {
      std::lock_guard<std::mutex> lk(m_);
      closed_ = true;
    }
    not_full_.notify_all();
    not_empty_.notify_all();
  }

  size_t size() const {
    std::lock_guard<std::mutex> lk(m_);
    return q_.size();
  }

 private:
  size_t max_items_;
  MemoryTracker& mem_;
  mutable std::mutex m_;
  std::condition_variable not_full_, not_empty_;
  std::deque<std::pair<T, int64_t>> q_;
  bool closed_ = false;
};

// Exponentially smoothed ingest bandwidth estimate, updated per chunk so the
// read pace tracks continuously varying link bandwidth.
class BandwidthEstimator {
 public:
  void update(int64_t bytes, int64_t elapsed_ms) {
    if (bytes <= 0 || elapsed_ms <= 0) return;
    double instant = static_cast<double>(bytes) * 8.0 / elapsed_ms;  // kbps
    if (!started_) {
      kbps_ = instant;
      started_ = true;
    } else {
      kbps_ = 0.7 * kbps_ + 0.3 * instant;
    }
  }
  double kbps() const { return kbps_; }

 private:
  bool started_ = false;
  double kbps_ = 0.0;
};

}  // namespace relay
