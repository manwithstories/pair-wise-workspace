// mediacore/bounded_queue.hpp — 固定容量阻塞队列（背压原语）
//
// 流水线解复用线程与封装线程之间的唯一通道：
//   * 容量固定为 capacity 个块，push 满时阻塞上游 -> 慢速写入下内存有界；
//   * cancel() 后所有阻塞的 push/pop 立即解除并返回 false，及时停止读取；
//   * peakSize() 记录运行期峰值，验收需证明不超过配置容量。
#pragma once

#include <condition_variable>
#include <cstddef>
#include <mutex>
#include <queue>

#include "mediacore/pcm.hpp"

namespace mediacore {

class BoundedBlockQueue {
public:
    explicit BoundedBlockQueue(std::size_t capacity)
        : capacity_(capacity) {}

    // 入队；队列满则阻塞。返回 false 表示已取消，调用方必须停止读取。
    bool push(AudioBlock block) {
        std::unique_lock lock(mutex_);
        notFull_.wait(lock, [&] { return closed_ || cancelled_ ||
                                       queue_.size() < capacity_; });
        if (closed_ || cancelled_) {
            return false;
        }
        queue_.push(std::move(block));
        if (queue_.size() > peak_) {
            peak_ = queue_.size();
        }
        notEmpty_.notify_one();
        return true;
    }

    // 出队；队列空则阻塞。返回 false 表示队列已关闭且排空。
    bool pop(AudioBlock& out) {
        std::unique_lock lock(mutex_);
        notEmpty_.wait(lock,
                       [&] { return closed_ || cancelled_ ||
                                    !queue_.empty(); });
        if (queue_.empty()) {
            return false; // 关闭/取消且已排空
        }
        out = std::move(queue_.front());
        queue_.pop();
        notFull_.notify_one();
        return true;
    }

    // 生产者正常结束时调用：排空后 pop 返回 false
    void close() {
        std::lock_guard lock(mutex_);
        closed_ = true;
        notEmpty_.notify_all();
        notFull_.notify_all();
    }

    void cancel() {
        std::lock_guard lock(mutex_);
        cancelled_ = true;
        closed_ = true;
        notEmpty_.notify_all();
        notFull_.notify_all();
    }

    std::size_t peakSize() const {
        std::lock_guard lock(mutex_);
        return peak_;
    }
    std::size_t capacity() const noexcept { return capacity_; }

private:
    mutable std::mutex mutex_;
    std::condition_variable notFull_;
    std::condition_variable notEmpty_;
    std::queue<AudioBlock> queue_;
    std::size_t capacity_;
    std::size_t peak_ = 0;
    bool closed_ = false;
    bool cancelled_ = false;
};

} // namespace mediacore
