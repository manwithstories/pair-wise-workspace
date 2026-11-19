// 共享媒体包类型：各端回传包在流水线中的统一表示。
#pragma once

#include <cstdint>

namespace media {

enum class TrackKind : uint8_t { Audio, Video, Subtitle };

// 统一时间基准：90 kHz 有理数时基（与 MPEG 体系一致），全流水线共用。
inline constexpr int64_t kMasterClockHz = 90000;

struct MediaPacket {
    uint64_t sequence = 0;        // 发送端单调序号（可能回绕，由上层处理）
    TrackKind track = TrackKind::Audio;
    int64_t pts_ticks = 0;        // 已换算到统一时基的展示时间
    int64_t arrival_ms = 0;       // 到达时刻（本地单调毫秒，抖动统计用）
    uint32_t payload_bytes = 0;   // 有效载荷字节数
    uint64_t payload_hash = 0;    // 载荷摘要（模拟校验/异或冗余）
    bool is_parity = false;       // 前向冗余校验包
    uint32_t fec_group = 0;       // 所属冗余组（sequence / kFecDataPerGroup）
};

// 前向冗余参数：每 8 个数据包配 2 个校验包，可恢复组内至多 2 个丢失。
inline constexpr uint32_t kFecDataPerGroup = 8;
inline constexpr uint32_t kFecParityPerGroup = 2;

} // namespace media
