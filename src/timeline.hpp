// timeline.hpp — 统一时间基准、序号/时间戳回绕、乱序/重复/丢包与错误模型
#pragma once
#include "common.hpp"
#include <map>
#include <optional>

namespace md {

// 每个 SSRC 一条归一化状态
struct StreamState {
    uint32_t ssrc = 0;
    uint32_t clock_rate = 90000;
    bool have_last = false;
    uint16_t last_seq = 0;
    uint32_t last_ts = 0;
    uint64_t ext_packets = 0;      // 扩展序号计数
    uint64_t ext_ts = 0;           // 扩展 RTP 时间戳
    // RTCP 时钟锚点：ntp_us 对应 rtp_ts（扩展值在 ext_ts_at_anchor）
    bool have_anchor = false;
    uint64_t ext_ts_anchor = 0;
    int64_t ntp_anchor_us = 0;
    int64_t max_drift_us = 0;
    bool untrusted = false;        // 当前处于不可信区间
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

    // 喂入一条原始 RTP（按文件到达顺序），输出 0..n 条按序归一化包
    void push(MediaPacket p, std::vector<MediaPacket>& out,
              std::vector<PipelineError>& errors);

    // 冲刷重排缓存（输入结束）
    void flush(std::vector<MediaPacket>& out,
               std::vector<PipelineError>& errors);

    // 注册 RTCP 发送者报告，建立/校正统一时间基准
    void apply_rtcp(const RtcpReport& rr, std::vector<PipelineError>& errors);

    const TimelineCounters& counters() const { return counters_; }

private:
    StreamState& state_for(uint32_t ssrc, uint32_t clock_rate);
    void assign_unified(MediaPacket& p, StreamState& st);
    bool seen_(uint32_t ssrc, uint16_t seq) const;

    const ProcessConfig& cfg_;
    std::map<uint32_t, StreamState> states_;
    // 重排缓存：key=(ssrc, 扩展序号)
    std::map<std::pair<uint32_t, uint64_t>, MediaPacket> pending_;
    // 每 SSRC 下一个期望交付的扩展序号
    std::map<uint32_t, uint64_t> next_ext_;
    // 已交付序号记录（有界：只保留最近窗口）
    std::map<uint32_t, std::map<uint16_t, uint8_t>> delivered_history_;
    TimelineCounters counters_;
};

} // namespace md
