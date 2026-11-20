#pragma once

// Minimal self-contained SHA-256 (FIPS 180-4). Used for input binding in
// checkpoints and for deterministic output checksums; no external deps.

#include <array>
#include <cstdint>
#include <string>
#include <string_view>

namespace relay {

class Sha256 {
 public:
  Sha256() { reset(); }
  void reset();
  void update(const void* data, size_t len);
  void update(std::string_view s) { update(s.data(), s.size()); }
  std::array<uint8_t, 32> final();
  static std::string hex(const std::array<uint8_t, 32>& digest);
  static std::string hex_of(std::string_view s) {
    Sha256 h;
    h.update(s);
    return hex(h.final());
  }

 private:
  void block(const uint8_t* p);
  uint32_t h_[8];
  uint8_t buf_[64];
  size_t buf_len_ = 0;
  uint64_t total_ = 0;
};

}  // namespace relay
