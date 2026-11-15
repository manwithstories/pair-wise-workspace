#pragma once
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace smx {
struct Cluster {
  std::u32string cps;
  std::size_t byte_start = 0;
  std::size_t byte_end = 0;
  std::uint32_t first_cp = 0;
};

struct Utf8DecodeResult {
  std::u32string text;
  std::vector<Cluster> clusters;
  std::size_t invalid_sequences = 0;
  std::string error;
};

Utf8DecodeResult decode_utf8(std::string_view input);
bool is_combining_mark(std::uint32_t cp);
bool is_zero_width_cp(std::uint32_t cp);
bool is_cjk_cp(std::uint32_t cp);
std::string utf8_encode(std::uint32_t cp);
std::string utf8_encode(const std::u32string& text);
}  // namespace smx
