#include "media/checksum.hpp"

namespace media {

std::uint64_t fnv1a64(const std::uint8_t* data, std::size_t size) noexcept {
  constexpr std::uint64_t kOffset = 0xcbf29ce484222325ull;
  constexpr std::uint64_t kPrime = 0x100000001b3ull;
  std::uint64_t h = kOffset;
  for (std::size_t i = 0; i < size; ++i) {
    h ^= static_cast<std::uint64_t>(data[i]);
    h *= kPrime;
  }
  return h;
}

std::uint32_t crc32(const std::uint8_t* data, std::size_t size) noexcept {
  static std::uint32_t table[256];
  static bool initialized = false;
  if (!initialized) {
    for (std::uint32_t i = 0; i < 256; ++i) {
      std::uint32_t c = i;
      for (int k = 0; k < 8; ++k)
        c = (c & 1u) ? (0xedb88320u ^ (c >> 1)) : (c >> 1);
      table[i] = c;
    }
    initialized = true;
  }
  std::uint32_t crc = 0xffffffffu;
  for (std::size_t i = 0; i < size; ++i)
    crc = table[(crc ^ data[i]) & 0xffu] ^ (crc >> 8);
  return crc ^ 0xffffffffu;
}

} // namespace media
