#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <string_view>

namespace relay {

/// Lower-case hexadecimal SHA-256 (64 characters).
std::string sha256_hex(std::span<const std::uint8_t> bytes);
std::string sha256_hex(std::string_view text);

/// Streaming helper for large inputs such as whole capture streams.
class Sha256Stream {
 public:
  Sha256Stream();
  ~Sha256Stream();
  Sha256Stream(const Sha256Stream&) = delete;
  Sha256Stream& operator=(const Sha256Stream&) = delete;

  void update(std::span<const std::uint8_t> bytes);
  void update(std::string_view text);
  [[nodiscard]] std::string finish_hex();

 private:
  void* ctx_ = nullptr;
  bool finished_ = false;
};

[[nodiscard]] bool is_hex_digest(std::string_view value);

}  // namespace relay
