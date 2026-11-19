#include "media/packet_reorder.hpp"

namespace media {

std::vector<MediaPacket> PacketReorder::push(const MediaPacket& pkt,
                                             ErrorModel& errors) {
    if (!started_) {
        next_expected_ = pkt.sequence;
        started_ = true;
    }

    if (pkt.sequence < next_expected_) {
        // 已提交区域：窗口内视为重复，窗口外视为迟到；两者都只记录不提交。
        if (next_expected_ - pkt.sequence <= window_) {
            duplicates_++;
            errors.record({ErrorKind::Duplicate, "", static_cast<int64_t>(pkt.sequence),
                           -1, -1, "duplicate of committed packet"});
        } else {
            late_dropped_++;
            errors.record({ErrorKind::LateOutsideWindow, "",
                           static_cast<int64_t>(pkt.sequence), -1, -1,
                           "late packet outside reorder window, dropped"});
        }
        return {};
    }

    auto [it, inserted] = pending_.emplace(pkt.sequence, pkt);
    if (!inserted) {
        // 序号已在窗口内：若时间戳也一致为重复包，否则为序号冲突（不确定性）。
        duplicates_++;
        bool same_ts = it->second.pts_ticks == pkt.pts_ticks;
        errors.record({same_ts ? ErrorKind::Duplicate : ErrorKind::Uncertain, "",
                       static_cast<int64_t>(pkt.sequence), -1, -1,
                       same_ts ? "duplicate packet (seq+pts match)"
                               : "sequence reuse with different pts"});
        return {};
    }

    if (pkt.sequence != next_expected_) {
        reordered_++;
        errors.record({ErrorKind::OutOfOrder, "", static_cast<int64_t>(pkt.sequence),
                       -1, -1, "buffered for reorder"});
    }

    // 窗口约束：超出窗口说明出现序号缺口，按丢失处理并推进提交点。
    if (pending_.size() > window_) {
        uint64_t gap_end = pending_.begin()->first;
        for (uint64_t s = next_expected_; s < gap_end; ++s)
            errors.record({ErrorKind::Lost, "", static_cast<int64_t>(s), -1, -1,
                           "sequence gap exceeds reorder window"});
        next_expected_ = gap_end;
    }
    return drain();
}

std::vector<MediaPacket> PacketReorder::drain() {
    std::vector<MediaPacket> out;
    while (true) {
        auto it = pending_.find(next_expected_);
        if (it == pending_.end()) break;
        out.push_back(std::move(it->second));
        pending_.erase(it);
        next_expected_++;
        committed_++;
    }
    return out;
}

std::vector<MediaPacket> PacketReorder::flush() {
    std::vector<MediaPacket> out;
    for (auto& [seq, pkt] : pending_) {
        out.push_back(std::move(pkt));
        committed_++;
    }
    pending_.clear();
    return out;
}

} // namespace media
