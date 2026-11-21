#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace relay {

struct DecodedFrameInfo {
  bool ok = false;
  int width = 0;
  int height = 0;
  std::string error;
};

// Decodes an encoded image payload with FFmpeg and validates it.
DecodedFrameInfo decode_validate(const std::vector<uint8_t>& payload);

// Encodes a deterministic RGB test pattern as PNG (fixture generation).
std::vector<uint8_t> encode_test_pattern_png(int width, int height,
                                             uint64_t seed);

} // namespace relay
