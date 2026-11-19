#include "media/recovery.hpp"

#include <algorithm>
#include <map>

namespace media {

GroupRecovery recover_group(uint32_t group_id,
                            const std::vector<MediaPacket>& data_present,
                            uint32_t parity_present, uint32_t group_size,
                            RecoveryStats& stats, ErrorModel& errors,
                            const std::string& source_track) {
    GroupRecovery result;
    const uint64_t group_start =
        static_cast<uint64_t>(group_id) * kFecDataPerGroup;

    std::map<uint64_t, MediaPacket> present;
    for (const auto& p : data_present) present.emplace(p.sequence, p);

    uint64_t group_end = group_start + group_size - 1;

    std::vector<uint64_t> missing;
    for (uint64_t s = group_start; s <= group_end; ++s)
        if (!present.count(s)) missing.push_back(s);

    if (missing.empty()) return result;

    stats.lost_packets += missing.size();

    if (missing.size() <= parity_present) {
        // 冗余足够：重建丢失包。载荷摘要以校验包异或链确定性重建
        // （模拟 (8,2) 里德-所罗门冗余的等价可恢复性）。
        uint64_t parity_hash = 0;
        for (const auto& p : data_present)
            if (p.is_parity) parity_hash ^= p.payload_hash;
        for (const auto& [seq, p] : present) parity_hash ^= p.payload_hash;

        for (uint64_t s : missing) {
            MediaPacket pkt;
            pkt.sequence = s;
            pkt.track = data_present.empty() ? TrackKind::Video
                                             : data_present.front().track;
            pkt.fec_group = group_id;
            // 时间戳按组内相邻包线性内插。
            auto upper = present.upper_bound(s);
            if (upper != present.begin() && upper != present.end()) {
                auto lower = std::prev(upper);
                int64_t span = static_cast<int64_t>(upper->first - lower->first);
                int64_t off = static_cast<int64_t>(s - lower->first);
                pkt.pts_ticks = lower->second.pts_ticks +
                                (upper->second.pts_ticks - lower->second.pts_ticks) *
                                    off / span;
                pkt.payload_bytes = lower->second.payload_bytes;
            } else if (!present.empty()) {
                pkt.pts_ticks = present.begin()->second.pts_ticks;
                pkt.payload_bytes = present.begin()->second.payload_bytes;
            }
            pkt.payload_hash = parity_hash ^ (s * 0x9E3779B97F4A7C15ULL);
            result.recovered.push_back(pkt);
            stats.recovered_packets++;
            errors.record({ErrorKind::Recovered, source_track,
                           static_cast<int64_t>(s), -1, -1,
                           "recovered via forward redundancy"});
        }
        std::sort(result.recovered.begin(), result.recovered.end(),
                  [](const MediaPacket& a, const MediaPacket& b) {
                      return a.sequence < b.sequence;
                  });
        return result;
    }

    // 冗余不足：写明确缺口，不伪造样本。缺口合并为连续区间。
    stats.unrecoverable_packets += missing.size();
    uint64_t run_start = missing.front(), prev = missing.front();
    auto emit_gap = [&](uint64_t a, uint64_t b) {
        Gap g;
        g.start_seq = a;
        g.end_seq = b;
        auto lo = present.lower_bound(a);
        if (lo != present.end()) g.start_ticks = lo->second.pts_ticks;
        auto hi = present.lower_bound(b);
        if (hi != present.end()) g.end_ticks = hi->second.pts_ticks;
        result.gaps.push_back(g);
        stats.gaps.push_back(g);
        for (uint64_t s = a; s <= b; ++s)
            errors.record({ErrorKind::Unrecoverable, source_track,
                           static_cast<int64_t>(s), -1, -1,
                           "insufficient redundancy, gap marked"});
    };
    for (size_t i = 1; i < missing.size(); ++i) {
        if (missing[i] != prev + 1) {
            emit_gap(run_start, prev);
            run_start = missing[i];
        }
        prev = missing[i];
    }
    emit_gap(run_start, prev);
    return result;
}

} // namespace media
