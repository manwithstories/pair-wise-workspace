#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

namespace media_restore {

// FNV-1a 64.  Chosen because it is trivially reproducible across builds,
// platforms and process runs, which is what the archive requires.
class StableHash {
 public:
  void update(const void* data, size_t size);
  void update(std::string_view text) { update(text.data(), text.size()); }
  void update_int64(int64_t value);
  uint64_t value() const { return state_; }
  std::string hex() const;

  static std::string hash_bytes(const void* data, size_t size);
  static std::string hash_text(std::string_view text);

 private:
  uint64_t state_ = 1469598103934665603ULL;
};

}  // namespace media_restore
