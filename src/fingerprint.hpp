// fingerprint.hpp — 逐片段稳定指纹与跨机位去重
#pragma once
#include "common.hpp"
#include <vector>
#include <optional>

namespace md {

struct Fingerprint {
    std::string sha256_hex;                  // 片段规范化字节流哈希
    std::string sample_summary_hex;          // 规范化采样摘要（碰撞复核）
};

// 计算片段指纹：规范化字节 = 包统一时间(LE64) + 载荷，逐包追加
Fingerprint compute_fingerprint(const std::vector<MediaPacket>& packets,
                                const std::vector<uint32_t>& audio_digest);

// 去重登记项
struct DedupEntry {
    uint32_t segment_id;
    std::string sha256_hex;
    std::string sample_summary_hex;
};

class Deduper {
public:
    // 返回重复的原片段 id；新内容返回 nullopt。
    // 哈希相同先视为碰撞候选，必须再比对规范化采样摘要确认。
    std::optional<uint32_t> check_and_add(const SegmentReport& seg);

private:
    std::vector<DedupEntry> entries_;
};

} // namespace md
