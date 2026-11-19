// 丢包恢复：对突发丢失执行前向冗余（FEC）恢复。
// 冗余可覆盖时重建丢失包；冗余不足时写入明确缺口（Gap），
// 绝不伪造媒体样本，并统计恢复率。
#pragma once

#include "media/error_model.hpp"
#include "media/packet.hpp"

#include <cstdint>
#include <vector>

namespace media {

// 不可恢复区间：[start_seq, end_seq] 闭区间 + 主时基时间范围。
struct Gap {
    uint64_t start_seq = 0;
    uint64_t end_seq = 0;
    int64_t start_ticks = 0;
    int64_t end_ticks = 0;
};

struct GroupRecovery {
    std::vector<MediaPacket> recovered; // 重建的包（按序号升序）
    std::vector<Gap> gaps;              // 本组内的不可恢复缺口
};

struct RecoveryStats {
    uint64_t lost_packets = 0;
    uint64_t recovered_packets = 0;
    uint64_t unrecoverable_packets = 0;
    std::vector<Gap> gaps;

    double recovery_rate() const {
        return lost_packets == 0
                   ? 1.0
                   : static_cast<double>(recovered_packets) / lost_packets;
    }
};

// 恢复一个 FEC 组。data_present 为组内已收到的数据包（可乱序），
// parity_present 为收到的校验包数，group_size 为组内应有数据包数
// （末组可能不足 kFecDataPerGroup）。模型为 (8,2) 冗余：
// 组内丢失数 <= parity_present 时全部重建，否则整段记为缺口。
GroupRecovery recover_group(uint32_t group_id,
                            const std::vector<MediaPacket>& data_present,
                            uint32_t parity_present, uint32_t group_size,
                            RecoveryStats& stats, ErrorModel& errors,
                            const std::string& source_track = "");

} // namespace media
