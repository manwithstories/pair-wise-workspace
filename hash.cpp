#include "hash.hpp"

#include <algorithm>
#include <cstdio>

namespace bake {

namespace {

constexpr uint64_t kFnvOffset = 14695981039346656037ULL;
constexpr uint64_t kFnvPrime = 1099511628211ULL;

void mixByte(uint64_t& hash, uint8_t byte) {
    hash ^= static_cast<uint64_t>(byte);
    hash *= kFnvPrime;
}

void mixU64(uint64_t& hash, uint64_t value) {
    for (int shift = 0; shift < 64; shift += 8) {
        mixByte(hash, static_cast<uint8_t>((value >> shift) & 0xffU));
    }
}

}  // namespace

uint64_t fnv1a64(const FrameBuffer& frame) {
    uint64_t hash = kFnvOffset;
    mixU64(hash, static_cast<uint64_t>(frame.width));
    mixU64(hash, static_cast<uint64_t>(frame.height));
    for (int y = 0; y < frame.height; ++y) {
        const uint8_t* row = frame.rowPtr(y);
        for (int x = 0; x < frame.width * 3; ++x) {
            mixByte(hash, row[x]);
        }
    }
    return hash;
}

std::string hashHex(uint64_t value) {
    char buffer[17];
    std::snprintf(buffer, sizeof(buffer), "%016llx",
                  static_cast<unsigned long long>(value));
    return std::string(buffer, 16);
}

FrameDiffStats diffFrames(const FrameBuffer& a, const FrameBuffer& b) {
    if (!sameDimensions(a, b)) {
        throw BakeError(ExitCode::invariant, "frame diff dimension mismatch");
    }
    FrameDiffStats stats;
    stats.sample_count = static_cast<uint64_t>(a.width) *
                         static_cast<uint64_t>(a.height) * 3ULL;
    for (int y = 0; y < a.height; ++y) {
        const uint8_t* row_a = a.rowPtr(y);
        const uint8_t* row_b = b.rowPtr(y);
        for (int x = 0; x < a.width; ++x) {
            uint32_t pixel_max = 0;
            for (int channel = 0; channel < 3; ++channel) {
                const uint8_t av = row_a[x * 3 + channel];
                const uint8_t bv = row_b[x * 3 + channel];
                const uint32_t difference = av > bv ? av - bv : bv - av;
                stats.sum_abs += difference;
                stats.max_abs = std::max(stats.max_abs, difference);
                pixel_max = std::max(pixel_max, difference);
                ++stats.histogram[std::min<std::size_t>(
                    difference / 16U, kDiffHistogramBins - 1)];
            }
            if (pixel_max != 0) ++stats.changed_pixels;
        }
    }
    return stats;
}

}  // namespace bake
