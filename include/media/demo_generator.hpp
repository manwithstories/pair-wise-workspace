// 演示数据生成：确定性（种子驱动）合成包轨迹与字幕样例。
// 注入重复、乱序、突发丢失与带宽变化；字幕含损坏/截断/重叠记录。
#pragma once

#include "media/error_model.hpp"
#include "media/packet.hpp"

#include <cstdint>
#include <string>
#include <vector>

namespace media {

struct DemoConfig {
    uint64_t seed = 41;
    uint64_t bandwidth_kbps = 3200;
    uint64_t target_kbps = 1800;
    uint32_t checkpoint_packets = 256;
    std::string output_dir;
};

struct DemoTrace {
    std::vector<MediaPacket> arrivals; // 含乱序/重复；丢失包不出现
    std::string srt_content;
    std::string vtt_content;
    uint64_t total_data_packets = 0; // 发送端发出的数据包总数（含被丢的）
    uint64_t lost_in_channel = 0;    // 信道丢弃数（供恢复模块处理）
    int64_t duration_ticks = 0;
};

DemoTrace generate_demo_trace(const DemoConfig& cfg);

} // namespace media
