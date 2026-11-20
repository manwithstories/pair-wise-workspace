#include "relay/hash.hpp"

#include <openssl/evp.h>

#include <array>
#include <cctype>
#include <stdexcept>

namespace relay {
namespace {

std::string hex(const unsigned char* data, unsigned int size) {
  static constexpr char kHex[] = "0123456789abcdef";
  std::string out;
  out.resize(static_cast<std::size_t>(size) * 2);
  for (unsigned int i = 0; i < size; ++i) {
    out[2 * i] = kHex[data[i] >> 4];
    out[2 * i + 1] = kHex[data[i] & 0x0f];
  }
  return out;
}

}  // namespace

std::string sha256_hex(std::span<const std::uint8_t> bytes) {
  std::array<unsigned char, EVP_MAX_MD_SIZE> digest{};
  unsigned int length = 0;
  EVP_MD_CTX* ctx = EVP_MD_CTX_new();
  if (ctx == nullptr) {
    throw std::runtime_error("EVP_MD_CTX_new failed");
  }
  const bool ok = EVP_DigestInit_ex(ctx, EVP_sha256(), nullptr) == 1 &&
                  EVP_DigestUpdate(ctx, bytes.data(), bytes.size()) == 1 &&
                  EVP_DigestFinal_ex(ctx, digest.data(), &length) == 1;
  EVP_MD_CTX_free(ctx);
  if (!ok) {
    throw std::runtime_error("SHA-256 computation failed");
  }
  return hex(digest.data(), length);
}

std::string sha256_hex(std::string_view text) {
  return sha256_hex(std::span<const std::uint8_t>(
      reinterpret_cast<const std::uint8_t*>(text.data()), text.size()));
}

Sha256Stream::Sha256Stream() {
  EVP_MD_CTX* ctx = EVP_MD_CTX_new();
  if (ctx == nullptr || EVP_DigestInit_ex(ctx, EVP_sha256(), nullptr) != 1) {
    if (ctx != nullptr) {
      EVP_MD_CTX_free(ctx);
    }
    throw std::runtime_error("SHA-256 stream init failed");
  }
  ctx_ = ctx;
}

Sha256Stream::~Sha256Stream() {
  if (ctx_ != nullptr && !finished_) {
    EVP_MD_CTX_free(static_cast<EVP_MD_CTX*>(ctx_));
  }
}

void Sha256Stream::update(std::span<const std::uint8_t> bytes) {
  if (finished_ || ctx_ == nullptr) {
    throw std::runtime_error("SHA-256 stream already finalized");
  }
  if (!bytes.empty() &&
      EVP_DigestUpdate(static_cast<EVP_MD_CTX*>(ctx_), bytes.data(),
                       bytes.size()) != 1) {
    throw std::runtime_error("SHA-256 stream update failed");
  }
}

void Sha256Stream::update(std::string_view text) {
  update(std::span<const std::uint8_t>(
      reinterpret_cast<const std::uint8_t*>(text.data()), text.size()));
}

std::string Sha256Stream::finish_hex() {
  if (finished_ || ctx_ == nullptr) {
    throw std::runtime_error("SHA-256 stream finalized twice");
  }
  std::array<unsigned char, EVP_MAX_MD_SIZE> digest{};
  unsigned int length = 0;
  const bool ok = EVP_DigestFinal_ex(static_cast<EVP_MD_CTX*>(ctx_),
                                     digest.data(), &length) == 1;
  EVP_MD_CTX_free(static_cast<EVP_MD_CTX*>(ctx_));
  ctx_ = nullptr;
  finished_ = true;
  if (!ok) {
    throw std::runtime_error("SHA-256 stream finalize failed");
  }
  return hex(digest.data(), length);
}

bool is_hex_digest(std::string_view value) {
  if (value.size() != 64) {
    return false;
  }
  for (const char c : value) {
    if (!std::isxdigit(static_cast<unsigned char>(c))) {
      return false;
    }
  }
  return true;
}

}  // namespace relay
