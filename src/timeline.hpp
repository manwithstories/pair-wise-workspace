// timeline.hpp — 统一时间基准、序号/时间戳回绕、乱序/重复/丢包与错误模型
#pragma once
#include "common.hpp"
#include <map>
#include <vector>

namespace md {

// 每个 SSRC 一条归一化状态
struct StreamState {
    uint32_t ssrc = 0;
    uint32_t clock_rate = 90000;
    uint64_t next_ext_seq = 0;   // 下一个期望交付的扩展序号
    uint64_t max_observed = 0;   // 已观测到的最大扩展序号（重排判定）
    bool have_observed = false;
    uint64_t ext_packets = 0;    // 已交付包数
    // 32 位时间戳扩展：显式记录回绕周期数，RTP/RTCP 共用
    bool have_ts_base = false;
    uint64_t wrap_count = 0;     // 已跨越的 32 位回绕次数
    uint64_t last_ext_ts = 0;    // 上一个观测/交付包的扩展时间戳
    // RTCP 时钟锚点：ntp_us 对应扩展 RTP 时间戳 ext_ts_anchor
    bool have_anchor = false;
    uint64_t ext_ts_anchor = 0;
    int64_t ntp_anchor_us = 0;
    int64_t max_drift_us = 0;
    bool untrusted = false;      // 当前处于不可信区间
};

struct TimelineCounters {
    uint32_t duplicates = 0;
    uint32_t losses = 0;
    uint32_t reordered = 0;
    uint32_t untrusted_spans = 0;
    uint32_t drift_violations = 0;
};

class TimelineNormalizer {
public:
    explicit TimelineNormalizer(const ProcessConfig& cfg) : cfg_(cfg) {}

    // 喂入一条原始 RTP（按文件到达顺序），按序归一化包追加到 out
    void push(MediaPacket p, std::vector<MediaPacket>& out,
              std::vector<PipelineError>& errors);

    // 冲刷重排缓存（输入结束）：缓存中残留均按序交付
    void flush(std::vector<MediaPacket>& out,
               std::vector<PipelineError>& errors);

    // 注册 RTCP 发送者报告，建立/校正统一时间基准
    void apply_rtcp(const RtcpReport& rr, std::vector<PipelineError>& errors);

    const TimelineCounters& counters() const { return counters_; }

private:
    struct Pending {
        MediaPacket pkt;
        uint64_t ext_seq;
        uint32_t wire_ts;   // 原始 32 位 ts，交付时才做回绕扩展
    };
    StreamState& state_for(uint32_t ssrc, uint32_t clock_rate);
    uint64_t extend_seq(StreamState& st, uint16_t seq) const;
    // 按序交付时刻：基于已确认时间线把 32 位 ts 扩展为 64 位并推进回绕计数
    uint64_t resolve_ts(StreamState& st, uint32_t ts);
    void assign_unified(MediaPacket& p, const StreamState& st, uint64_t ext_ts);
    void deliver(StreamState& st, Pending pn, std::vector<MediaPacket>& out,
                 std::vector<PipelineError>& errors);

    const ProcessConfig& cfg_;
    std::map<uint32_t, StreamState> states_;
    // 重排缓存（有界窗口）：key=(ssrc, 扩展序号)
    std::map<std::pair<uint32_t, uint64_t>, Pending> pending_;
    // 每 SSRC 最近已交付序号（重复检测，有界历史）
    std::map<uint32_t, std::map<uint64_t, uint8_t>> delivered_history_;
    TimelineCounters counters_;
};

} // namespace md
