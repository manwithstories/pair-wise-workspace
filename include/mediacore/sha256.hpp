#pragma once

#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <string>
#include <string_view>

namespace mediacore {

// Self-contained SHA-256 (FIPS 180-4). No third-party dependency so the
// audit stays fully offline and deterministic.
class Sha256 {
public:
    Sha256();
    void update(const std::uint8_t* data, std::size_t len);
    void update(std::string_view data) {
        update(reinterpret_cast<const std::uint8_t*>(data.data()),
               data.size());
    }
    // Returns 64 lowercase hex characters and resets internal state.
    std::string hexDigest();

private:
    void compressBlock(const std::uint8_t* block);
    std::uint32_t state_[8];
    std::uint64_t bitLength_ = 0;
    std::uint8_t buffer_[64]{};
    std::size_t bufferLen_ = 0;
};

// Stream a file through the hash in fixed-size chunks so per-track memory
// stays bounded. Returns nullopt when the file cannot be read.
std::optional<std::string> hashFile(const std::filesystem::path& path,
                                    std::size_t chunkBytes = 1 << 16);

}  // namespace mediacore
