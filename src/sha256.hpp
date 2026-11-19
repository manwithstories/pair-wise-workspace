// sha256.hpp -- streaming SHA-256 used for input checksums and output hashes.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <string>

namespace mediaqc {

class Sha256 {
 public:
  Sha256();
  void update(const void* data, size_t len);
  void update(const std::string& s) { update(s.data(), s.size()); }
  // Finalises the digest; the object must not be updated afterwards.
  std::string hex_digest();
  const std::array<uint8_t, 32>& digest();

 private:
  void compress(const uint8_t* block);
  std::array<uint32_t, 8> state_;
  std::array<uint8_t, 64> buffer_{};
  uint64_t total_bytes_ = 0;
  size_t buffered_ = 0;
  std::array<uint8_t, 32> digest_{};
  bool finalized_ = false;
};

// Hash of a whole file, streamed in bounded chunks.  Returns false on I/O
// error; `bytes` receives the number of bytes read.
bool sha256_file(const std::filesystem::path& path, std::string& hex_out,
                 int64_t& bytes_out);

// Hash of the first `count` bytes (used for prefix checkpoints).
bool sha256_file_prefix(const std::filesystem::path& path, int64_t count,
                        std::string& hex_out);

}  // namespace mediaqc
