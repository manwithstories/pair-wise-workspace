#pragma once
#include <cstdint>
#include <string>
#include <vector>

#include "error.hpp"

// 8-bit binary-capable PPM (P6) reader/writer with strict validation.
//
// The tool only ever consumes and produces its own controlled PPMs, so the
// parser is intentionally narrow:
//   - magic P6, maxval exactly 255 (8 bits per channel),
//   - ASCII decimal header tokens separated by whitespace, '#' comments
//     allowed between tokens,
//   - exactly width*height*3 payload bytes, no trailing garbage,
//   - zero width/height rejected.
namespace lutgrade {

struct Image {
  int width = 0;
  int height = 0;
  // Interleaved RGB, size = width*height*3, each 0..255.
  std::vector<std::uint8_t> rgb;

  std::size_t pixel_count() const noexcept {
    return static_cast<std::size_t>(width) * static_cast<std::size_t>(height);
  }
};

// Path-based helpers. Both throw Error(kUsage, ...) on any I/O or format
// problem so callers keep a single error channel.
Image read_ppm(const std::string& path);
void write_ppm(const std::string& path, const Image& image);

// In-memory cores, exposed for tests and for generate-fixture.
Image decode_ppm(const std::string& bytes, const std::string& origin);
std::string encode_ppm(const Image& image);

}  // namespace lutgrade
