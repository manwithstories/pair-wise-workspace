// pipeline.hpp — 有界队列 + 背压的多阶段流水线
#pragma once
#include "common.hpp"
#include <thread>
#include <mutex>
#include <condition_variable>
#include <queue>
#include <atomic>

namespace md {

// 有界阻塞队列：满则阻塞生产者（背压），取消后 push/pop 立即失败
template <typename T>
class BoundedQueue {
public:
    explicit BoundedQueue(uint32_t cap) : cap_(cap) {}

    bool push(T item, const std::atomic<bool>& cancel) {
        std::unique_lock<std::mutex> lk(mu_);
        not_full_.wait(lk, [&] { return q_.size() < cap_ || cancel.load(); });
        if (cancel.load()) return false;
        q_.push(std::move(item));
        peak_ = std::max(peak_, uint32_t(q_.size()));
        not_empty_.notify_one();
        return true;
    }

    // 关闭并等待排空：pop 在空且 done 时返回 false
    void close() {
        { std::lock_guard<std::mutex> lk(mu_); done_ = true; }
        not_empty_.notify_all();
        not_full_.notify_all();
    }
    void cancel() {
        { std::lock_guard<std::mutex> lk(mu_); cancelled_ = true; done_ = true; }
        not_empty_.notify_all();
        not_full_.notify_all();
    }

    bool pop(T& out) {
        std::unique_lock<std::mutex> lk(mu_);
        not_empty_.wait(lk, [&] { return !q_.empty() || done_; });
        if (q_.empty()) return false;
        out = std::move(q_.front());
        q_.pop();
        not_full_.notify_one();
        return true;
    }

    uint32_t peak() const {
        std::lock_guard<std::mutex> lk(mu_);
        return peak_;
    }
    uint32_t capacity() const { return cap_; }

private:
    mutable std::mutex mu_;
    std::condition_variable not_full_, not_empty_;
    std::queue<T> q_;
    uint32_t cap_;
    uint32_t peak_ = 0;
    bool done_ = false;
    bool cancelled_ = false;
};

// 运行完整流水线；io_fail_path 非空时模拟该路径写入失败（测试用）
// crash_after_segment > 0 时在落盘该编号片段后模拟中断（测试恢复用）
struct PipelineHooks {
    std::string io_fail_path;
    uint32_t crash_after_segment = 0;
};

ExitCode run_pipeline(const ProcessConfig& cfg,
                      const PipelineHooks& hooks,
                      PipelineResult& result);

} // namespace md
