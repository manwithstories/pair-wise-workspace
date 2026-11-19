#pragma once

// Small self-contained SHA-256 implementation. Keeping it in-tree avoids any
// dependency beyond the C++ standard library and makes the digest reproducible
// on both Apple Silicon and Intel macOS.

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string>

namespace mfs {

using Sha256Digest = std::array<uint8_t, 32>;

class Sha256 {
 public:
  Sha256();

  void update(std::span<const uint8_t> data);
  void update(const void* data, size_t size);

  // Finalises the digest. The object must not be updated afterwards.
  Sha256Digest finish();

  static Sha256Digest hash(std::span<const uint8_t> data);
  static Sha256Digest hash(const std::string& text);
  static std::string hex(const Sha256Digest& digest);

 private:
  void compress(const uint8_t* block);

  std::array<uint32_t, 8> state_{};
  std::array<uint8_t, 64> buffer_{};
  size_t buffered_{0};
  uint64_t total_bytes_{0};
  bool finished_{false};
};

}  // namespace mfs
