#include "timeline.hpp"
#include "util.hpp"
#include <algorithm>

namespace md {

namespace {
// RTP 时钟滴答 -> 微秒（__int128 防止长时长溢出）
int64_t ticks_to_us(int64_t ticks, uint32_t clock_rate) {
    return int64_t((__int128(ticks) * 1'000'000) / clock_rate);
}
} // namespace

StreamState& TimelineNormalizer::state_for(uint32_t ssrc, uint32_t clock_rate) {    auto [it, inserted] = states_.emplace(ssrc, StreamState{});
    if (inserted) {
        it->second.ssrc = ssrc;
        it->second.clock_rate = clock_rate ? clock_rate : 90000u;
    }
    return it->second;
}

uint64_t TimelineNormalizer::extend_seq(StreamState& st, uint16_t seq) const {
    // 以 next_ext_seq 所在 64K 周期为基准解释 16 位序号
    uint64_t base = st.next_ext_seq & ~uint64_t(0xFFFF);
    uint64_t cand = base | seq;
    int64_t diff = int64_t(cand) - int64_t(st.next_ext_seq);
    if (diff > int32_t(0x7FFF)) cand -= uint64_t(SEQ_MOD);       // 回绕到上一周期
    else if (diff < -int32_t(0x7FFF)) cand += uint64_t(SEQ_MOD);  // 跨到下一周期
    return cand;
}

uint64_t TimelineNormalizer::resolve_ts(StreamState& st, uint32_t ts) {
    // 仅在按序交付时刻调用：pending 已消除乱序，ext_seq 严格递增。
    // 在当前周期及其相邻周期中，选离已确认时间线 last_ext_ts 最近的解释。
    if (!st.have_ts_base) {
        st.have_ts_base = true;
        st.last_ext_ts = ts;
        return ts;
    }
    uint64_t cur = st.wrap_count * uint64_t(TS_MOD) + ts;
    uint64_t hi = cur + uint64_t(TS_MOD);
    uint64_t lo = st.wrap_count > 0 ? cur - uint64_t(TS_MOD) : cur;
    auto dist = [](uint64_t a, uint64_t b) { return a >= b ? a - b : b - a; };
    uint64_t best = cur;
    if (dist(hi, st.last_ext_ts) < dist(best, st.last_ext_ts)) {
        best = hi;
        st.wrap_count += 1;
        // 回绕后锚点若仍停留在旧周期，同步抬升，保证统一时间基准不回退
        if (st.have_anchor && st.ext_ts_anchor < cur)
            st.ext_ts_anchor += uint64_t(TS_MOD);
    } else if (st.wrap_count > 0 &&
               dist(lo, st.last_ext_ts) < dist(best, st.last_ext_ts)) {
        best = lo;
        st.wrap_count -= 1;
        if (st.have_anchor && st.ext_ts_anchor > cur)
            st.ext_ts_anchor -= uint64_t(TS_MOD);
    }
    st.last_ext_ts = best;
    return best;
}

void TimelineNormalizer::assign_unified(MediaPacket& p, const StreamState& st,
                                        uint64_t ext_ts) {
    if (st.have_anchor) {
        // RTCP 锚点：统一微秒 = 锚点 NTP + (扩展ts - 锚点ts) 按 clock 换算
        int64_t delta_ticks = int64_t(ext_ts) - int64_t(st.ext_ts_anchor);
        p.unified_us = st.ntp_anchor_us + ticks_to_us(delta_ticks, st.clock_rate);
    } else {
        // 无锚点时退化为本地线性映射（首包为 0），后续 RTCP 到达会重锚
        p.unified_us = ticks_to_us(int64_t(ext_ts), st.clock_rate);
    }
}

void TimelineNormalizer::deliver(StreamState& st, Pending pn,
                                 std::vector<MediaPacket>& out,
                                 std::vector<PipelineError>& errors) {
    MediaPacket& p = pn.pkt;
    // 丢包缺口：扩展序号不连续
    if (pn.ext_seq > st.next_ext_seq) {
        uint32_t gap = uint32_t(pn.ext_seq - st.next_ext_seq);
        counters_.losses += gap;
        if (!st.untrusted) {
            st.untrusted = true;
            ++counters_.untrusted_spans;
            errors.push_back({ErrType::UntrustedTime, stage::Timeline,
                              uint32_t(st.next_ext_seq & 0xFFFF),
                              "unrecoverable gap of " + std::to_string(gap) +
                                  " packet(s); time marked untrusted"});
        }
    } else if (pn.ext_seq == st.next_ext_seq) {
        st.untrusted = false; // 重新对齐，退出不可信区间
    }
    // 按序交付时刻才解析时间戳，回绕计数不被乱序污染
    uint64_t ext_ts = resolve_ts(st, pn.wire_ts);
    assign_unified(p, st, ext_ts);
    p.untrusted = st.untrusted;
    st.next_ext_seq = pn.ext_seq + 1;
    ++st.ext_packets;
    out.push_back(std::move(p));
}

void TimelineNormalizer::push(MediaPacket p, std::vector<MediaPacket>& out,
                              std::vector<PipelineError>& errors) {
    StreamState& st = state_for(p.ssrc, p.clock_rate);
    if (st.ext_packets == 0 && !st.have_observed)
        st.next_ext_seq = p.seq; // 该 SSRC 首包序号作为扩展基准

    uint64_t ext_seq = extend_seq(st, p.seq);
    uint32_t wire_ts = p.rtp_ts;   // 保留原始 32 位，交付时再扩展

    // 重复投递：同一扩展序号再次到达
    auto& hist = delivered_history_[p.ssrc];
    if (hist.count(ext_seq)) {
        p.duplicate = true;
        ++counters_.duplicates;
        // 重复包不推进时间线：复用已确认的扩展时间戳
        assign_unified(p, st, st.last_ext_ts);
        p.untrusted = st.untrusted;
        out.push_back(std::move(p));
        return;
    }

    // 乱序：迟到包（序号小于已观测最大值），重复已在上方拦截
    if (st.have_observed && int64_t(ext_seq) < int64_t(st.max_observed))
        ++counters_.reordered;
    if (!st.have_observed || ext_seq > st.max_observed) st.max_observed = ext_seq;
    st.have_observed = true;

    Pending pn{std::move(p), ext_seq, wire_ts};
    pending_.emplace(std::make_pair(pn.pkt.ssrc, ext_seq), std::move(pn));
    hist[ext_seq] = 1;
    // 有界历史：只保留 2 倍重排窗口，旧序号不可能再重复到达
    while (hist.size() > size_t(cfg_.reorder_window) * 2)
        hist.erase(hist.begin());

    // 有界重排窗口：超出窗口即强制交付最旧项，缓存不随时长增长
    while (pending_.size() > cfg_.reorder_window) {
        auto it = pending_.begin();
        StreamState& old_st = states_[it->first.first];
        Pending old = std::move(it->second);
        pending_.erase(it);
        deliver(old_st, std::move(old), out, errors);
    }

    // 按全局 (ssrc, ext_seq) 顺序交付所有已到齐的连续项
    while (!pending_.empty()) {
        auto it = pending_.begin();
        StreamState& head_st = states_[it->first.first];
        if (it->first.second != head_st.next_ext_seq) break;
        Pending ready = std::move(it->second);
        pending_.erase(it);
        deliver(head_st, std::move(ready), out, errors);
    }
}

void TimelineNormalizer::flush(std::vector<MediaPacket>& out,
                               std::vector<PipelineError>& errors) {
    while (!pending_.empty()) {
        auto it = pending_.begin();
        StreamState& st = states_[it->first.first];
        Pending ready = std::move(it->second);
        pending_.erase(it);
        deliver(st, std::move(ready), out, errors);
    }
}

void TimelineNormalizer::apply_rtcp(const RtcpReport& rr,
                                    std::vector<PipelineError>& errors) {
    StreamState& st = state_for(rr.ssrc, 0);
    // RTCP 只读已确认周期，不推进回绕计数（推进只发生在按序交付）。
    // 把 32 位 ts 放在当前周期或下一周期中，取离已确认时间线最近者。
    uint64_t same = st.wrap_count * uint64_t(TS_MOD) + rr.rtp_ts;
    uint64_t ext_ts = same;
    if (st.have_ts_base) {
        uint64_t next_cycle = same + uint64_t(TS_MOD);
        auto dist = [](uint64_t a, uint64_t b) { return a >= b ? a - b : b - a; };
        if (dist(next_cycle, st.last_ext_ts) < dist(same, st.last_ext_ts))
            ext_ts = next_cycle;
    }
    if (!st.have_anchor) {
        st.have_anchor = true;
        st.ext_ts_anchor = ext_ts;
        st.ntp_anchor_us = rr.ntp_us;
        return;
    }
    // 漂移检测：RTCP 推算时间与锚点外推时间之差
    int64_t delta_ticks = int64_t(ext_ts) - int64_t(st.ext_ts_anchor);
    int64_t inferred = st.ntp_anchor_us + ticks_to_us(delta_ticks, st.clock_rate);
    int64_t drift = rr.ntp_us - inferred;
    if (drift < 0) drift = -drift;
    st.max_drift_us = std::max(st.max_drift_us, drift);
    if (drift > cfg_.drift_limit_us) {
        ++counters_.drift_violations;
        // 超限：保留原始时间（锚点不动），仅记录误差来源
        errors.push_back({ErrType::ClockDrift, stage::Timeline, 0,
                          "rtcp drift " + std::to_string(drift) +
                              "us exceeds limit; raw time retained"});
    }
}

} // namespace md
