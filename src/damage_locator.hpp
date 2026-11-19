#pragma once
// 损坏区间定位：解码失败/截断/损坏包后继续扫描，
// 输出区间边界（包序号+字节偏移+时间）与恢复成功包数。
#include "error_model.hpp"
#include "media_types.hpp"

#include <optional>
#include <vector>

namespace ma {

class DamageLocator {
public:
    explicit DamageLocator(int recoveryWindow = 8)
        : recoveryWindow_(recoveryWindow) {}

    // 读取/解码阶段的错误事件。
    struct ErrorEvent {
        ErrorCategory category;
        int64_t packetSeq = -1;
        int64_t filePos = -1;
        double timeMs = 0.0;
        std::string detail;
    };

    // 一个包处理完成后调用；success=false 表示该包解码失败。
    // 错误后继续扫描：错误包之后连续 recoveryWindow 个成功包即收敛区间。
    void onPacket(bool success, int64_t seq, int64_t filePos, double timeMs) {
        ++total_;
        if (success) {
            lastGoodSeq_ = seq;
            lastGoodPos_ = filePos;
            lastGoodMs_ = timeMs;
            if (inError_) {
                ++recovered_;
                ++recoveredSinceRange_;
                ++goodRun_;
                if (goodRun_ >= recoveryWindow_) closeRange();
            }
            return;
        }
        // 失败包计入 errorPackets；连续失败归并到同一损坏区间。
        ++errorPackets_;
        if (!inError_) openRange(seq, filePos, timeMs);
        ++curRangeErrorCount_;
        goodRun_ = 0;
        extendRange(seq, filePos, timeMs);
    }

    // 显式错误事件（读包错误、容器损坏、截断）。
    void onError(const ErrorEvent& ev) {
        ++total_;
        ++errorPackets_;
        if (!inError_) openRange(ev.packetSeq, ev.filePos, ev.timeMs);
        ++curRangeErrorCount_;
        goodRun_ = 0;
        curRange_.category = ev.category;
        if (!ev.detail.empty()) curRange_.detail = ev.detail;
        extendRange(ev.packetSeq, ev.filePos, ev.timeMs);
    }

    // EOF/扫描结束：截断类错误保留开放区间，其余收敛到最后已知位置。
    void finish(bool truncated) {
        if (!inError_) return;
        if (truncated) {
            curRange_.category = ErrorCategory::Truncated;
            curRange_.detail = curRange_.detail.empty()
                ? "文件截断：流在预期结束前终止" : curRange_.detail;
        }
        closeRange();
    }

    const std::vector<DamageRange>& ranges() const { return ranges_; }

    RecoveryRecord recovery() const {
        RecoveryRecord r;
        r.totalPackets = total_;
        r.errorPackets = errorPackets_;
        r.recoveredPackets = recovered_;
        return r;
    }

private:
    void openRange(int64_t seq, int64_t pos, double ms) {
        inError_ = true;
        curRange_ = DamageRange{};
        curRange_.category = ErrorCategory::DecodeFailed;
        curRange_.startPacket = seq;
        curRange_.startFilePos = pos;
        curRange_.startMs = ms;
        curRangeErrorCount_ = 0;
        recoveredSinceRange_ = 0;
    }
    void extendRange(int64_t seq, int64_t pos, double ms) {
        curRange_.endPacket = seq;
        curRange_.endFilePos = pos;
        curRange_.endMs = ms;
    }
    void closeRange() {
        curRange_.errorPackets = curRangeErrorCount_;
        curRange_.recoveredPackets = recoveredSinceRange_;
        ranges_.push_back(curRange_);
        inError_ = false;
        goodRun_ = 0;
        curRangeErrorCount_ = 0;
        recoveredSinceRange_ = 0;
    }

    int recoveryWindow_;
    bool inError_ = false;
    int curRangeErrorCount_ = 0;
    int goodRun_ = 0;
    int64_t recoveredSinceRange_ = 0;
    int64_t total_ = 0;
    int64_t errorPackets_ = 0;
    int64_t recovered_ = 0;
    int64_t lastGoodSeq_ = -1;
    int64_t lastGoodPos_ = -1;
    double lastGoodMs_ = 0.0;
    DamageRange curRange_;
    std::vector<DamageRange> ranges_;
};

} // namespace ma
