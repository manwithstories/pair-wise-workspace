#include "fingerprint.hpp"
#include "sha256.hpp"
#include "util.hpp"

namespace md {
namespace {
// FNV-1a 64 位滚动哈希，用于与主 SHA-256 独立的采样摘要路径
uint64_t fnv_init() { return 0xcbf29ce484222325ull; }
uint64_t fnv_byte(uint64_t h, uint8_t b) {
    h ^= b;
    h *= 0x100000001b3ull;
    return h;
}
void put_le64(std::vector<uint8_t>& out, uint64_t v) {
    for (int i = 0; i < 8; ++i) out.push_back(uint8_t(v >> (8 * i)));
}
} // namespace

Fingerprint compute_fingerprint(const std::vector<MediaPacket>& packets) {
    std::vector<uint8_t> canonical;
    Sha256 summary;   // 独立摘要：只吃规范化采样统计，不吃完整载荷
    uint64_t fnv = fnv_init();
    for (const auto& p : packets) {
        // 主指纹：统一时间(LE64) + 载荷长度(LE32) + 载荷，确定有序
        put_le64(canonical, uint64_t(p.unified_us));
        put_u32(canonical, uint32_t(p.payload.size()));
        canonical.insert(canonical.end(), p.payload.begin(), p.payload.end());

        // 采样摘要统计：长度、字节和、首尾字节（与主哈希输入不同）
        uint64_t byte_sum = 0;
        for (uint8_t b : p.payload) byte_sum += b;
        uint8_t front_b = p.payload.empty() ? uint8_t(0) : p.payload.front();
        uint8_t back_b = p.payload.empty() ? uint8_t(0) : p.payload.back();
        uint8_t stat[8] = {
            uint8_t(p.payload.size()), uint8_t(p.payload.size() >> 8),
            uint8_t(p.payload.size() >> 16), uint8_t(p.payload.size() >> 24),
            uint8_t(byte_sum), uint8_t(byte_sum >> 8),
            front_b, back_b};
        summary.update(stat, sizeof(stat));
        for (uint8_t b : stat) fnv = fnv_byte(fnv, b);
    }
    // FNV 混入摘要，避免两个统计前缀相同的片段产生相同摘要
    uint8_t fnv_le[8];
    for (int i = 0; i < 8; ++i) fnv_le[i] = uint8_t(fnv >> (8 * i));
    summary.update(fnv_le, sizeof(fnv_le));

    Fingerprint fp;
    fp.sha256_hex = Sha256::hex(Sha256::hash_bytes(canonical));
    fp.sample_summary_hex = Sha256::hex(summary.finish());
    return fp;
}

std::optional<uint32_t> Deduper::check_and_add(const SegmentReport& seg) {
    for (const auto& e : entries_) {
        if (e.sha256_hex != seg.sha256) continue;   // 哈希不同 => 内容不同
        // 哈希相同：碰撞候选，必须比对规范化采样摘要确认
        if (e.sample_summary_hex == seg.sample_summary)
            return e.segment_id;                    // 确认重复
        // 摘要不同 => 哈希碰撞，不判重，继续
    }
    entries_.push_back({seg.id, seg.sha256, seg.sample_summary});
    return std::nullopt;
}

} // namespace md
