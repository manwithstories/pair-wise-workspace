#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

namespace media_restore {

// Self-contained SHA-256 so segment hashes are stable across runs, machines
// and FFmpeg builds without pulling in an external crypto dependency.
class Sha256 {
 public:
  void update(const void* data, size_t size);
  void update(std::string_view text) { update(text.data(), text.size()); }
  std::string hex();

  static std::string digest(std::string_view text);

 private:
  void transform(const uint8_t block[64]);
  uint32_t state_[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
                        0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};
  uint64_t length_ = 0;
  uint8_t buffer_[64] = {};
  size_t buffered_ = 0;
};

}  // namespace media_restore
