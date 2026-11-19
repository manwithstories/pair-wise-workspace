// Small deterministic checksum helpers (FNV-1a 64 and CRC-32), no external deps.
#pragma once

#include <cstdint>
#include <cstddef>

namespace media {

std::uint64_t fnv1a64(const std::uint8_t* data, std::size_t size) noexcept;
std::uint32_t crc32(const std::uint8_t* data, std::size_t size) noexcept;

} // namespace media
