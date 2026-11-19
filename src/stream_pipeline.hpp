#pragma once
// 固定内存上限的流式管线：有界队列背压、阶段取消、超时与错误传播。
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <functional>
#include <mutex>
#include <optional>

namespace ma {

using SteadyClock = std::chrono::steady_clock;

// 协作式取消令牌（原子标志，队列等待以其为谓词）。
class CancellationToken {
public:
    void cancel() { cancelled_.store(true, std::memory_order_release); }
    bool cancelled() const {
        return cancelled_.load(std::memory_order_acquire);
    }

private:
    std::atomic<bool> cancelled_{false};
};

// 按字节计账的有界队列：满时阻塞上游形成背压。
template <typename T>
class BoundedQueue {
public:
    struct Entry {
        T value;
        std::size_t bytes;
    };

    BoundedQueue(std::size_t maxBytes, std::size_t maxItems = 4096)
        : maxBytes_(maxBytes), maxItems_(maxItems) {}

    // 返回 false 表示被取消（调用方应放弃阶段）。
    bool push(T item, std::size_t bytes, CancellationToken& token,
              int* backpressureEvents = nullptr) {
        std::unique_lock<std::mutex> lk(m_);
        if (bytes > maxBytes_) bytes = maxBytes_; // 单项超大仍允许通过
        bool blocked = false;
        while (!token.cancelled() &&
               (curBytes_ + bytes > maxBytes_ || q_.size() >= maxItems_)) {
            blocked = true;
            // 超时轮询取消令牌，保证取消在 ~50ms 内解除背压阻塞。
            notFull_.wait_for(lk, std::chrono::milliseconds(50));
        }
        if (token.cancelled()) return false;
        if (blocked && backpressureEvents) ++*backpressureEvents;
        q_.push_back(Entry{std::move(item), bytes});
        curBytes_ += bytes;
        peakBytes_ = std::max(peakBytes_, curBytes_);
        notEmpty_.notify_one();
        return true;
    }

    // 队列关闭且排空（或取消）后返回 nullopt。
    std::optional<T> pop(CancellationToken& token) {
        std::unique_lock<std::mutex> lk(m_);
        while (q_.empty() && !closed_ && !token.cancelled())
            notEmpty_.wait_for(lk, std::chrono::milliseconds(50));
        if (q_.empty()) return std::nullopt;
        Entry e = std::move(q_.front());
        q_.pop_front();
        curBytes_ -= std::min(curBytes_, e.bytes);
        notFull_.notify_one();
        return std::move(e.value);
    }

    void close() {
        {
            std::lock_guard<std::mutex> lk(m_);
            closed_ = true;
        }
        notEmpty_.notify_all();
        notFull_.notify_all();
    }
    std::size_t peakBytes() const { return peakBytes_; }

private:
    std::mutex m_;
    std::condition_variable notEmpty_, notFull_;
    std::deque<Entry> q_;
    std::size_t maxBytes_, maxItems_;
    std::size_t curBytes_ = 0;
    std::size_t peakBytes_ = 0;
    bool closed_ = false;
};

// 管线运行指标：延迟（首包读取→末包结果落盘）、背压、超时。
struct PipelineMetrics {
    std::vector<double> latencyMs;   // 每包端到端延迟样本
    int backpressureEvents = 0;
    bool timeout = false;
    bool cancelled = false;
    double latencyP95Ms() const;
    double peakRssMiB = 0.0;
};

// 阶段间错误传播：任一致命错误记录并触发取消。
class PipelineErrorState {
public:
    void fail(const std::string& msg) {
        {
            std::lock_guard<std::mutex> lk(m_);
            if (message_.empty()) message_ = msg;
        }
        token_.cancel();
    }
    bool failed() const { return token_.cancelled(); }
    std::string message() const {
        std::lock_guard<std::mutex> lk(m_);
        return message_;
    }
    CancellationToken& token() { return token_; }

private:
    mutable std::mutex m_;
    CancellationToken token_;
    std::string message_;
};

// 当前进程驻留内存（MiB），macOS 走 mach task_info。
double currentRssMiB();

} // namespace ma
