#pragma once

#include <condition_variable>
#include <cstdint>
#include <deque>
#include <mutex>
#include <optional>

namespace mediaqc {

// A blocking multi-producer, multi-consumer queue bounded by both a frame
// count and a total byte count.  Producers block once either limit is reached,
// so a slow consumer exerts backpressure instead of letting memory grow
// without bound.  `close()` wakes everyone; a cancelled queue rejects further
// pushes so an interrupt can unwind the pipeline deterministically.
template <typename T>
class BoundedQueue {
 public:
  BoundedQueue(std::uint64_t max_items, std::uint64_t max_bytes, std::uint64_t (*cost)(const T&))
      : max_items_(max_items), max_bytes_(max_bytes), cost_(cost) {}

  // Blocks until there is room.  Returns false if the queue was cancelled or
  // closed before room became available.
  bool push(T value) {
    std::unique_lock lock(mutex_);
    const std::uint64_t bytes = cost_(value);
    not_full_.wait(lock, [&] {
      return cancelled_ || (items_.size() < max_items_ &&
                            (bytes_ + bytes <= max_bytes_ || items_.empty()));
    });
    if (cancelled_) {
      return false;
    }
    items_.push_back(std::move(value));
    bytes_ += bytes;
    if (items_.size() > peak_items_) peak_items_ = items_.size();
    if (bytes_ > peak_bytes_) peak_bytes_ = bytes_;
    not_empty_.notify_one();
    return true;
  }

  // Blocks until an item is available or the queue is closed and drained.
  std::optional<T> pop() {
    std::unique_lock lock(mutex_);
    not_empty_.wait(lock, [&] { return cancelled_ || !items_.empty(); });
    if (items_.empty()) {
      return std::nullopt;  // cancelled and empty
    }
    T value = std::move(items_.front());
    items_.pop_front();
    bytes_ -= cost_(value);
    not_full_.notify_one();
    return value;
  }

  // Wakes both sides; no new items will be accepted.
  void close() {
    std::lock_guard lock(mutex_);
    closed_ = true;
    not_empty_.notify_all();
    not_full_.notify_all();
  }

  // Cancellation implies close and rejects pushes immediately.
  void cancel() {
    std::lock_guard lock(mutex_);
    cancelled_ = true;
    closed_ = true;
    not_empty_.notify_all();
    not_full_.notify_all();
  }

  [[nodiscard]] std::uint64_t peak_items() const {
    std::lock_guard lock(mutex_);
    return peak_items_;
  }
  [[nodiscard]] std::uint64_t peak_bytes() const {
    std::lock_guard lock(mutex_);
    return peak_bytes_;
  }
  [[nodiscard]] std::size_t size() const {
    std::lock_guard lock(mutex_);
    return items_.size();
  }
  [[nodiscard]] std::uint64_t bytes() const {
    std::lock_guard lock(mutex_);
    return bytes_;
  }
  [[nodiscard]] bool cancelled() const {
    std::lock_guard lock(mutex_);
    return cancelled_;
  }

 private:
  mutable std::mutex mutex_;
  std::condition_variable not_full_;
  std::condition_variable not_empty_;
  std::deque<T> items_;
  std::uint64_t max_items_;
  std::uint64_t max_bytes_;
  std::uint64_t (*cost_)(const T&);
  std::uint64_t bytes_ = 0;
  std::uint64_t peak_items_ = 0;
  std::uint64_t peak_bytes_ = 0;
  bool closed_ = false;
  bool cancelled_ = false;
};

}  // namespace mediaqc
