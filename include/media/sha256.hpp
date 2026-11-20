#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

namespace mediaqc {

using Sha256Digest = std::array<std::uint8_t, 32>;

// Streaming SHA-256.  Implemented locally so hashes do not depend on the
// host FFmpeg build and remain stable across releases.
class Sha256 {
 public:
  Sha256();

  void update(const void* data, std::size_t size);
  void update(std::string_view text);
  [[nodiscard]] Sha256Digest finish();

 private:
  void compress(const std::uint8_t* block);

  std::array<std::uint32_t, 8> state_{};
  std::array<std::uint8_t, 64> buffer_{};
  std::size_t buffer_len_ = 0;
  std::uint64_t total_len_ = 0;
  bool finished_ = false;
};

[[nodiscard]] Sha256Digest sha256(const void* data, std::size_t size);
[[nodiscard]] Sha256Digest sha256(std::string_view text);
[[nodiscard]] Sha256Digest sha256_of_file(const std::string& path, std::string& error);

[[nodiscard]] std::string to_hex(const Sha256Digest& digest);
[[nodiscard]] std::string to_hex(const std::uint8_t* data, std::size_t size);
// Parses exactly 64 hex digits.  Returns nullopt on any other shape.
[[nodiscard]] std::optional<Sha256Digest> parse_hex_digest(std::string_view hex);

}  // namespace mediaqc
