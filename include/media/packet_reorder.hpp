// 序号窗口、去重和乱序重排。
// 重复包按序号+时间戳识别；乱序仅在窗口内重排；
// 窗口外迟到包被丢弃并记录，绝不回退已提交的输出（检查点安全）。
#pragma once

#include "media/error_model.hpp"
#include "media/packet.hpp"

#include <cstdint>
#include <map>
#include <vector>

namespace media {

class PacketReorder {
  public:
    explicit PacketReorder(uint32_t window_size = 64) : window_(window_size) {}

    // 送入一个到达包，返回本次可提交（按序）的包序列。
    std::vector<MediaPacket> push(const MediaPacket& pkt, ErrorModel& errors);

    // 流结束：按序号提交窗口内剩余包。
    std::vector<MediaPacket> flush();

    uint64_t duplicates() const { return duplicates_; }
    uint64_t reordered() const { return reordered_; }
    uint64_t late_dropped() const { return late_dropped_; }
    uint64_t committed() const { return committed_; }

  private:
    std::vector<MediaPacket> drain();

    uint32_t window_;
    uint64_t next_expected_ = 0;
    bool started_ = false;
    std::map<uint64_t, MediaPacket> pending_;
    uint64_t duplicates_ = 0;
    uint64_t reordered_ = 0;
    uint64_t late_dropped_ = 0;
    uint64_t committed_ = 0;
};

} // namespace media
