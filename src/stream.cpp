#include "stream.hpp"

#include <algorithm>

#include "util.hpp"

namespace mb {

BoundedQueue::BoundedQueue(size_t capacity, size_t slow_consumer_ms)
    : cap_(std::clamp(capacity, size_t(1), kMaxQueueCapacity)),
      slow_ms_(slow_consumer_ms) {}

bool BoundedQueue::push(std::vector<uint8_t> item, size_t* depth_after) {
  std::unique_lock<std::mutex> lk(mu_);
  if (q_.size() >= cap_) {
    ++wait_count_;  // 触发背压：生产者必须等待，不能丢弃也不能无限堆积
    not_full_.wait(lk, [&] { return closed_ || q_.size() < cap_; });
  }
  if (closed_) return false;
  cur_bytes_ += item.size();
  q_.push_back(std::move(item));
  ++pushed_;
  peak_items_ = std::max(peak_items_, q_.size());
  peak_bytes_ = std::max(peak_bytes_, cur_bytes_);
  if (depth_after) *depth_after = q_.size();
  lk.unlock();
  not_empty_.notify_one();
  return true;
}

std::optional<std::vector<uint8_t>> BoundedQueue::pop() {
  if (slow_ms_ > 0) std::this_thread::sleep_for(
      std::chrono::milliseconds(slow_ms_));
  std::unique_lock<std::mutex> lk(mu_);
  not_empty_.wait(lk, [&] { return closed_ || !q_.empty(); });
  if (q_.empty()) return std::nullopt;  // 已关闭且排空
  std::vector<uint8_t> item = std::move(q_.front());
  cur_bytes_ -= std::min<uint64_t>(cur_bytes_, item.size());
  q_.pop_front();
  lk.unlock();
  not_full_.notify_one();
  return item;
}

void BoundedQueue::close() {
  {
    std::lock_guard<std::mutex> lk(mu_);
    closed_ = true;
  }
  not_empty_.notify_all();
  not_full_.notify_all();
}

bool BoundedQueue::closed() const {
  std::lock_guard<std::mutex> lk(mu_);
  return closed_;
}

size_t BoundedQueue::peak_items() const {
  std::lock_guard<std::mutex> lk(mu_);
  return peak_items_;
}
uint64_t BoundedQueue::peak_bytes() const {
  std::lock_guard<std::mutex> lk(mu_);
  return peak_bytes_;
}
uint64_t BoundedQueue::total_pushed() const {
  std::lock_guard<std::mutex> lk(mu_);
  return pushed_;
}
uint64_t BoundedQueue::push_wait_count() const {
  std::lock_guard<std::mutex> lk(mu_);
  return wait_count_;
}
size_t BoundedQueue::capacity() const { return cap_; }

}  // namespace mb
