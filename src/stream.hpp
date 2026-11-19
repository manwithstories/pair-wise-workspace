// 有界队列与背压：固定容量、阻塞式生产/消费，任何阶段都不会无限缓存。
// 记录峰值队列长度与峰值内存估算（按入队项声明的字节数累计）。
#pragma once
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <mutex>
#include <optional>
#include <thread>
#include <vector>

namespace mb {

class BoundedQueue {
 public:
  // capacity 必须在 [1, kMaxQueueCapacity] 内，否则夹取到合法区间。
  explicit BoundedQueue(size_t capacity, size_t slow_consumer_ms = 0);

  // 阻塞入队；队列关闭后返回 false。返回当前（入队后）队列长度。
  bool push(std::vector<uint8_t> item, size_t* depth_after = nullptr);
  // 阻塞出队；关闭且排空后返回 nullopt。
  std::optional<std::vector<uint8_t>> pop();
  void close();           // 通知消费者不会再有数据
  bool closed() const;

  size_t peak_items() const;
  uint64_t peak_bytes() const;     // 峰值驻留字节（仅数据负载）
  uint64_t total_pushed() const;
  uint64_t push_wait_count() const;  // 因满而阻塞的次数（背压证据）
  size_t capacity() const;

 private:
  mutable std::mutex mu_;
  std::condition_variable not_full_, not_empty_;
  std::deque<std::vector<uint8_t>> q_;
  size_t cap_;
  size_t slow_ms_;                 // 模拟慢消费者：pop 前额外睡眠
  size_t peak_items_ = 0;
  uint64_t cur_bytes_ = 0, peak_bytes_ = 0;
  uint64_t pushed_ = 0, wait_count_ = 0;
  bool closed_ = false;
};

// 流水线背压统计汇总，供报告模块消费。
struct StreamStats {
  size_t capacity = 0;
  size_t peak_items = 0;
  uint64_t peak_bytes = 0;
  uint64_t total_pushed = 0;
  uint64_t push_waits = 0;
};

}  // namespace mb
