// sha256.hpp — 纯本地实现的 SHA-256（无外部依赖，确定性输出）
#pragma once
#include <cstdint>
#include <string>
#include <array>
#include <vector>
#include <cstddef>

namespace md {

class Sha256 {
public:
    Sha256() { reset(); }
    void reset();
    void update(const uint8_t* data, size_t len);
    void update(const std::vector<uint8_t>& data) { update(data.data(), data.size()); }
    std::array<uint8_t, 32> finish();

    static std::array<uint8_t, 32> hash_bytes(const uint8_t* data, size_t len);
    static std::array<uint8_t, 32> hash_bytes(const std::vector<uint8_t>& data) {
        return hash_bytes(data.data(), data.size());
    }
    static std::string hex(const std::array<uint8_t, 32>& digest);

private:
    uint32_t state_[8]{};
    uint8_t buffer_[64]{};
    uint64_t bitlen_ = 0;
    size_t buflen_ = 0;
};

} // namespace md
