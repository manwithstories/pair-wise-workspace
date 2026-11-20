#pragma once
// FIPS 180-4 SHA-256, portable C++20, no external dependency.
#include <array>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace mb {

class Sha256 {
 public:
  Sha256() { reset(); }
  void reset();
  void update(const std::uint8_t* data, std::size_t len);
  void update(std::string_view s) {
    update(reinterpret_cast<const std::uint8_t*>(s.data()), s.size());
  }
  std::array<std::uint8_t, 32> finish();
  static std::array<std::uint8_t, 32> hash(const std::vector<std::uint8_t>& b) {
    Sha256 h;
    h.update(b.data(), b.size());
    return h.finish();
  }
  static std::string hex(const std::array<std::uint8_t, 32>& digest);

 private:
  void compress(const std::uint8_t block[64]);
  std::array<std::uint32_t, 8> state_{};
  std::uint64_t bit_len_ = 0;
  std::array<std::uint8_t, 64> buffer_{};
  std::size_t buffer_len_ = 0;
};

}  // namespace mb
