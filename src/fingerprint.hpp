// fingerprint.hpp — 逐片段稳定指纹与跨机位去重
#pragma once
#include "common.hpp"
#include <optional>
#include <vector>

namespace md {

struct Fingerprint {
    std::string sha256_hex;         // 片段规范化字节流哈希（64 位十六进制）
    std::string sample_summary_hex; // 规范化采样摘要（碰撞复核，独立于主哈希）
};

// 计算片段指纹。
// 规范化字节流：逐包写入 统一时间(LE64) + 载荷长度(LE32) + 载荷，保证确定有序。
// 采样摘要：对每包的规范化采样统计（长度/字节和/首尾字节）独立滚动哈希，
// 与主哈希算法路径不同，用于哈希碰撞时的二次复核。
Fingerprint compute_fingerprint(const std::vector<MediaPacket>& packets);

struct DedupEntry {
    uint32_t segment_id;
    std::string sha256_hex;
    std::string sample_summary_hex;
};

class Deduper {
public:
    // 返回重复的原片段 id；新内容返回 nullopt。
    // 哈希相同先视为碰撞候选，必须再比对规范化采样摘要确认后才判重。
    std::optional<uint32_t> check_and_add(const SegmentReport& seg);

private:
    std::vector<DedupEntry> entries_;
};

} // namespace md
