#pragma once
#include <cstdint>
#include <string>
#include <vector>

namespace smx {
struct SynthFontResult {
  bool ok = false;
  std::string error;
  std::vector<std::uint8_t> bytes;
  std::vector<std::uint32_t> codepoints;
};

SynthFontResult synth_font(std::uint64_t seed);
bool write_synth_font(const std::string& path, std::uint64_t seed, std::string& error);
}  // namespace smx
