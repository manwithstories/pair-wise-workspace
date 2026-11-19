#pragma once
// 统一媒体时间基准：回绕 PTS/DTS 展开、重复/倒退/缺失识别，保留原始值供复核。
#include <algorithm>
#include <cstdint>
#include <vector>

namespace ma {

// 时间戳异常分类。
enum class TsKind {
    Normal,
    Wrap,       // 检测到回绕并已展开
    Repeat,     // 与上一帧相同
    Backward,   // 非回绕性倒退
    Missing,    // 间隔显著超出预期
};

// 单次时间戳事件：同时保留原始值与展开值。
struct TsEvent {
    int64_t raw = 0;
    int64_t expanded = 0;
    int64_t deltaTicks = 0;  // 相对上一原始时间戳的差值（未展开）
    TsKind kind = TsKind::Normal;
};

// 单条流的时间戳展开/异常状态机。
// modulus 为回绕模数（33 位时间戳默认 2^33）；所有展开值均为单调域 int64。
class StreamClock {
public:
    // wrapOnly=true 时仅报告大跨度回绕（2^31 级），忽略 B 帧解码序导致的
    // 小范围 PTS 重排（Repeat/Backward），用于 PTS 呈现时间回绕检测。
    explicit StreamClock(int64_t modulus = 1LL << 33,
                         double missingFactor = 3.0,
                         bool wrapOnly = false)
        : modulus_(modulus), missingFactor_(missingFactor),
          wrapOnly_(wrapOnly) {}

    // 向下取整除法（模数恒为正）。
    static int64_t floorDiv(int64_t a, int64_t b) {
        int64_t q = a / b;
        int64_t r = a % b;
        if (r != 0 && ((r < 0) != (b < 0))) --q;
        return q;
    }

    // 喂入一个原始 PTS/DTS，返回展开后的值并记录异常事件。
    int64_t feed(int64_t raw) {
        TsEvent ev;
        ev.raw = raw;
        if (!started_) {
            started_ = true;
            prevRaw_ = raw;
            prevExpanded_ = raw;
            offset_ = 0;
            ev.expanded = raw;
            events_.push_back(ev);
            return raw;
        }
        ev.deltaTicks = raw - prevRaw_;
        // wrapOnly 模式基于原始值跨度判定（展开域不抬升）；
        // 普通模式基于展开域判定是否需要抬升模数。
        const int64_t backwardSpan = wrapOnly_
            ? (raw < prevRaw_ ? prevRaw_ - raw : 0)
            : (prevExpanded_ - raw);
        const int64_t monotonicNeed = backwardSpan;
        // wrapOnly（PTS 回绕检测）：纯绝对阈值，不依赖被 B 帧重排污染的 EWMA。
        // 普通模式：半模数，或 1000×典型间隔且 >1e6 ticks 的大跨度倒退。
        const bool isWrap = wrapOnly_
            ? (monotonicNeed > (1LL << 30))
            : (monotonicNeed > modulus_ / 2 ||
               (expectedDelta_ > 0 &&
                monotonicNeed > 1000 * expectedDelta_ &&
                monotonicNeed > 1000000));
        if (monotonicNeed > 0 && ev.deltaTicks < 0 && isWrap) {
            ev.kind = TsKind::Wrap;
            ++wrapCount_;
        } else if (raw == prevRaw_) {
            if (!wrapOnly_) ev.kind = TsKind::Repeat;
        } else if (!wrapOnly_ && monotonicNeed > 0) {
            ev.kind = TsKind::Backward;
        } else if (!wrapOnly_ && expectedDelta_ > 0 &&
                   static_cast<double>(ev.deltaTicks) >
                       missingFactor_ * static_cast<double>(expectedDelta_)) {
            ev.kind = TsKind::Missing;
        }
        if (ev.kind == TsKind::Normal || ev.kind == TsKind::Missing) {
            // EWMA 自适应期望间隔（VFR 下跟踪典型间隔）。
            expectedDelta_ = expectedDelta_ == 0
                ? ev.deltaTicks
                : static_cast<int64_t>(0.9 * static_cast<double>(expectedDelta_) +
                                       0.1 * static_cast<double>(ev.deltaTicks));
        }
        // 展开为严格单调域：取满足 expanded > 上一展开值的最小模数倍数。
        // 用向下取整除法（C++ 原生整除向零截断，负数场景会错）。
        // wrapOnly 模式仅做回绕检测、不参与毫秒换算，小重排不抬升展开域，
        // 避免 B 帧倒退污染 prevExpanded_ 后把后续帧误判为巨型回绕。
        int64_t k = 0;
        if (!wrapOnly_) {
            const int64_t numerator = prevExpanded_ - raw;
            k = floorDiv(numerator, modulus_) + 1;
            if (k < 0) k = 0;
        }
        offset_ = k * modulus_;
        prevRaw_ = raw;
        prevExpanded_ = raw + offset_;
        ev.expanded = prevExpanded_;
        events_.push_back(ev);
        return ev.expanded;
    }

    void reset() { *this = StreamClock(modulus_, missingFactor_); }

    int wrapCount() const { return wrapCount_; }
    const std::vector<TsEvent>& events() const { return events_; }
    int64_t expectedDelta() const { return expectedDelta_; }

    // ticks（按 timeBase 计）转毫秒。
    static double toMs(int64_t ticks, int timeBaseNum, int timeBaseDen) {
        if (timeBaseNum <= 0 || timeBaseDen <= 0) return 0.0;
        return static_cast<double>(ticks) * 1000.0 *
               static_cast<double>(timeBaseNum) /
               static_cast<double>(timeBaseDen);
    }

private:
    int64_t modulus_;
    double missingFactor_;
    bool wrapOnly_;
    bool started_ = false;
    int64_t prevRaw_ = 0;
    int64_t prevExpanded_ = 0;
    int64_t offset_ = 0;
    int64_t expectedDelta_ = 0;
    int wrapCount_ = 0;
    std::vector<TsEvent> events_;
};

// 百分位（输入会被排序），p 取 [0,100]；空向量返回 0。
inline double percentileMs(std::vector<double> values, double p) {
    if (values.empty()) return 0.0;
    std::sort(values.begin(), values.end());
    const double idx = (p / 100.0) * static_cast<double>(values.size() - 1);
    const auto lo = static_cast<size_t>(idx);
    const auto hi = std::min(lo + 1, values.size() - 1);
    const double frac = idx - static_cast<double>(lo);
    return values[lo] * (1.0 - frac) + values[hi] * frac;
}

} // namespace ma
