#include "fingerprint.hpp"
// 批次 6 实现
namespace md {
Fingerprint compute_fingerprint(const std::vector<MediaPacket>&,
                                const std::vector<uint32_t>&) { return {}; }
std::optional<uint32_t> Deduper::check_and_add(const SegmentReport&) {
    return std::nullopt;
}
} // namespace md
