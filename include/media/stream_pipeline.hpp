#pragma once
// 有界包队列与背压：队满阻塞生产者；下游失败时取消并释放所有等待线程。
#include <condition_variable>
#include <cstddef>
#include <deque>
#include <functional>
#include <mutex>
#include <optional>
#include "media/packet.hpp"
#include "media/time_model.hpp"

namespace media {

enum class PushResult { Ok, Cancelled };
enum class PopResult { Ok, Closed, Cancelled };

template <typename T> class BoundedQueue {
public:
  explicit BoundedQueue(size_t capacity) : capacity_(capacity) {}

  PushResult push(T item) {
    std::unique_lock lock(mu_);
    notFull_.wait(lock, [&] { return cancelled_ || closed_ ||
                                  q_.size() < capacity_; });
    if (cancelled_) return PushResult::Cancelled;
    q_.push_back(std::move(item));
    peak_ = std::max(peak_, q_.size());
    notEmpty_.notify_one();
    return PushResult::Ok;
  }

  // 生产者结束：关闭队列，消费者排空后得到 Closed。
  void close() {
    std::lock_guard lock(mu_);
    closed_ = true;
    notEmpty_.notify_all();
    notFull_.notify_all();
  }

  // 下游致命错误：取消全部等待，排队数据丢弃。
  void cancel(TimeError reason) {
    std::lock_guard lock(mu_);
    cancelled_ = true;
    cancelReason_ = reason;
    notEmpty_.notify_all();
    notFull_.notify_all();
  }

  std::pair<PopResult, std::optional<T>> pop() {
    std::unique_lock lock(mu_);
    notEmpty_.wait(lock, [&] { return cancelled_ || closed_ || !q_.empty(); });
    if (cancelled_) return {PopResult::Cancelled, std::nullopt};
    if (q_.empty()) return {PopResult::Closed, std::nullopt};
    T item = std::move(q_.front());
    q_.pop_front();
    notFull_.notify_one();
    return {PopResult::Ok, std::move(item)};
  }

  size_t peak() const {
    std::lock_guard lock(mu_);
    return peak_;
  }
  bool cancelled() const {
    std::lock_guard lock(mu_);
    return cancelled_;
  }
  TimeError cancelReason() const {
    std::lock_guard lock(mu_);
    return cancelReason_;
  }
  size_t capacity() const { return capacity_; }

private:
  mutable std::mutex mu_;
  std::condition_variable notFull_, notEmpty_;
  std::deque<T> q_;
  size_t capacity_;
  size_t peak_ = 0;
  bool closed_ = false;
  bool cancelled_ = false;
  TimeError cancelReason_ = TimeError::None;
};

using PacketQueue = BoundedQueue<MediaPacket>;

struct PipelineStats {
  size_t produced = 0;
  size_t consumed = 0;
  size_t peak = 0;
  TimeError error = TimeError::None;
  size_t firstBadPacket = 0;
};

// 单轨流式管线：生产者逐包推入有界队列（队满背压），消费者逐包处理；
// 消费者返回致命错误时取消队列并释放阻塞中的生产者。
PipelineStats runPipeline(PacketQueue& q,
                          const std::vector<MediaPacket>& source,
                          const std::function<TimeError(MediaPacket&)>& consume);
} // namespace media
