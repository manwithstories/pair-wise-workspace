#include "hash.hpp"

#include <cstdio>

namespace media_restore {
namespace {
constexpr uint64_t kPrime = 1099511628211ULL;
}

void StableHash::update(const void* data, size_t size) {
  const auto* bytes = static_cast<const unsigned char*>(data);
  for (size_t i = 0; i < size; ++i) {
    state_ ^= bytes[i];
    state_ *= kPrime;
  }
}

void StableHash::update_int64(int64_t value) {
  // Explicit little-endian encoding so hosts with different byte order agree.
  unsigned char buffer[8];
  const uint64_t bits = static_cast<uint64_t>(value);
  for (int i = 0; i < 8; ++i) buffer[i] = static_cast<unsigned char>(bits >> (8 * i));
  update(buffer, sizeof(buffer));
}

std::string StableHash::hex() const {
  char buffer[17];
  std::snprintf(buffer, sizeof(buffer), "%016llx",
                static_cast<unsigned long long>(state_));
  return std::string(buffer);
}

std::string StableHash::hash_bytes(const void* data, size_t size) {
  StableHash hash;
  hash.update(data, size);
  return hash.hex();
}

std::string StableHash::hash_text(std::string_view text) {
  return hash_bytes(text.data(), text.size());
}

}  // namespace media_restore
