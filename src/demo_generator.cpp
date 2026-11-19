#include "media/demo_generator.hpp"

#include "media/checkpoint_index.hpp"
#include "media/timebase.hpp"

#include <algorithm>
#include <random>

namespace media {
namespace {

// 生成 SRT：正常记录 + 一条损坏时间轴 + 一条截断记录。
std::string make_srt(int64_t duration_ms) {
    std::string s;
    int idx = 1;
    for (int64_t t = 0; t + 2000 <= duration_ms; t += 4000, ++idx) {
        auto fmt = [](int64_t ms) {
            char buf[32];
            std::snprintf(buf, sizeof(buf), "%02lld:%02lld:%02lld,%03lld",
                          ms / 3600000, ms / 60000 % 60, ms / 1000 % 60,
                          ms % 1000);
            return std::string(buf);
        };
        s += std::to_string(idx) + "\n" + fmt(t) + " --> " + fmt(t + 1800) +
             "\n对话行 " + std::to_string(idx) + "\n\n";
    }
    // 损坏记录：时间轴行非法。
    s += std::to_string(idx) + "\n99:99:99:99 --> xx\n损坏的时间轴\n\n";
    ++idx;
    // 截断记录：只有时间轴没有正文，且位于文件尾。
    s += std::to_string(idx) + "\n00:00:01,000 --> 00:00:02,000\n";
    return s;
}

// 生成 WebVTT：含重叠区间（供重叠策略处理）。
std::string make_vtt(int64_t duration_ms) {
    std::string s = "WEBVTT\n\n";
    auto fmt = [](int64_t ms) {
        char buf[32];
        std::snprintf(buf, sizeof(buf), "%02lld:%02lld:%02lld.%03lld",
                      ms / 3600000, ms / 60000 % 60, ms / 1000 % 60, ms % 1000);
        return std::string(buf);
    };
    int idx = 1;
    for (int64_t t = 1000; t + 1500 <= duration_ms; t += 5000, ++idx) {
        s += fmt(t) + " --> " + fmt(t + 2500) + "\n环境声标注 " +
             std::to_string(idx) + "\n\n";
        // 每 4 条插入一条与前一条重叠的。
        if (idx % 4 == 0)
            s += fmt(t + 2000) + " --> " + fmt(t + 3600) + "\n重叠标注\n\n";
    }
    return s;
}

} // namespace

DemoTrace generate_demo_trace(const DemoConfig& cfg) {
    DemoTrace trace;
    std::mt19937_64 rng(cfg.seed);

    const int64_t duration_ms = 30000; // 30 秒素材
    trace.duration_ticks = ms_to_ticks(duration_ms);
    const uint64_t payload = 1200;
    const double pps = static_cast<double>(cfg.target_kbps) * 125.0 / payload;
    const uint64_t total = static_cast<uint64_t>(pps * duration_ms / 1000.0);
    trace.total_data_packets = total;

    // 起点时间码 23:59:50:00 @ 30000/1001：10 秒后跨日界，演练回绕。
    FrameUnwrapper unwrapper(kFps2997);
    Timecode start_tc{23, 59, 50, 0};
    auto start_frame = timecode_to_frame(start_tc, kFps2997);

    // 多个包共享同一帧（每帧约 6 个包）：帧号 -> 主时基 PTS。
    const int64_t total_frames =
        static_cast<int64_t>(duration_ms) * 30000 / 1001 / 1000;

    std::vector<MediaPacket> sent;
    sent.reserve(total);
    for (uint64_t i = 0; i < total; ++i) {
        MediaPacket p;
        p.sequence = i;
        p.track = (i % 6 == 5) ? TrackKind::Audio : TrackKind::Video;
        int64_t frame =
            *start_frame + static_cast<int64_t>(i) * total_frames / static_cast<int64_t>(total);
        int64_t pts = frame_to_ticks(
            unwrapper.unwrap(frame % frames_per_day(kFps2997)), kFps2997);
        if (p.track == TrackKind::Audio)
            pts += ms_to_ticks(12) + static_cast<int64_t>(i) / 200; // 偏移+漂移
        p.pts_ticks = pts;
        p.payload_bytes = static_cast<uint32_t>(
            payload + static_cast<int64_t>(rng() % 160) - 80);
        p.payload_hash = fnv1a64(&i, sizeof(i), cfg.seed);
        p.fec_group = static_cast<uint32_t>(i / kFecDataPerGroup);
        sent.push_back(p);
    }

    // 信道损伤：突发丢失（每组至多 2 个，保证 FEC 可恢复）。
    std::vector<bool> dropped(total, false);
    std::uniform_int_distribution<uint64_t> group_pick(0, total / kFecDataPerGroup - 1);
    for (int burst = 0; burst < 24; ++burst) {
        uint64_t g = group_pick(rng);
        uint64_t a = g * kFecDataPerGroup + rng() % kFecDataPerGroup;
        uint64_t b = g * kFecDataPerGroup + rng() % kFecDataPerGroup;
        if (!dropped[a]) { dropped[a] = true; trace.lost_in_channel++; }
        if (!dropped[b]) { dropped[b] = true; trace.lost_in_channel++; }
    }

    // 到达序列：乱序（窗口内交换）+ 重复 + 带宽变化下的到达时刻。
    std::vector<MediaPacket> wire;
    for (uint64_t i = 0; i < total; ++i)
        if (!dropped[i]) wire.push_back(sent[i]);
    // 组间插入 FEC 校验包（每满一组数据包后附上 2 个校验包）。
    std::vector<MediaPacket> with_parity;
    with_parity.reserve(wire.size() + total / kFecDataPerGroup * 2);
    uint64_t since_group_start = 0;
    uint32_t cur_group = 0;
    uint64_t parity_hash[2] = {0, 0};
    auto flush_parity = [&](uint32_t g) {
        for (int k = 0; k < 2; ++k) {
            MediaPacket par;
            par.sequence = static_cast<uint64_t>(g) * kFecDataPerGroup;
            par.is_parity = true;
            par.fec_group = g;
            par.payload_hash = parity_hash[k];
            with_parity.push_back(par);
        }
        parity_hash[0] = parity_hash[1] = 0;
    };
    for (const auto& p : wire) {
        if (p.fec_group != cur_group) {
            flush_parity(cur_group);
            cur_group = p.fec_group;
            since_group_start = 0;
        }
        with_parity.push_back(p);
        parity_hash[since_group_start % 2] ^= p.payload_hash;
        since_group_start++;
    }
    flush_parity(cur_group);

    // 乱序：窗口内相邻交换。
    for (size_t i = 1; i + 1 < with_parity.size(); ++i)
        if (!with_parity[i].is_parity && !with_parity[i + 1].is_parity &&
            rng() % 100 < 12)
            std::swap(with_parity[i], with_parity[i + 1]);
    // 重复：约 1.5% 的数据包重发一次。
    for (size_t i = 0; i < with_parity.size(); ++i)
        if (!with_parity[i].is_parity && rng() % 1000 < 15)
            with_parity.insert(with_parity.begin() + static_cast<long>(i) + 1,
                               with_parity[i]);

    // 到达时刻：带宽前高后低（中段突降），含抖动。
    int64_t t = 0;
    for (size_t i = 0; i < with_parity.size(); ++i) {
        double bw = static_cast<double>(cfg.bandwidth_kbps);
        if (i > with_parity.size() / 3 && i < with_parity.size() / 2)
            bw *= 0.4; // 带宽突降区
        double interval_ms = with_parity[i].payload_bytes * 8.0 / bw;
        t += static_cast<int64_t>(interval_ms + static_cast<double>(rng() % 6));
        with_parity[i].arrival_ms = t;
    }

    trace.arrivals = std::move(with_parity);
    trace.srt_content = make_srt(duration_ms);
    trace.vtt_content = make_vtt(duration_ms);
    return trace;
}

} // namespace media
