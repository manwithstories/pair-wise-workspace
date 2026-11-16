#include "image_io.hpp"

#include <cctype>
#include <fstream>
#include <limits>
#include <sstream>

namespace lutgrade {
namespace {

[[noreturn]] void bad(const std::string& origin, const std::string& what) {
  throw Error(ExitCode::kUsage, "ppm_invalid", origin + ": " + what);
}

bool is_space(unsigned char c) {
  return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' ||
         c == '\f';
}

struct HeaderCursor {
  const std::string& text;
  std::size_t pos = 0;

  // Skip whitespace and '#' comments. A comment runs to end of line.
  void skip_filler() {
    while (pos < text.size()) {
      const unsigned char c = static_cast<unsigned char>(text[pos]);
      if (is_space(c)) {
        ++pos;
      } else if (c == '#') {
        while (pos < text.size() && text[pos] != '\n' && text[pos] != '\r') ++pos;
      } else {
        break;
      }
    }
  }

  // Read one unsigned decimal token. Rejects empty, non-digit, overflow.
  std::uint64_t read_uint(const std::string& origin, const char* field) {
    skip_filler();
    const std::size_t start = pos;
    while (pos < text.size() &&
           std::isdigit(static_cast<unsigned char>(text[pos]))) {
      ++pos;
    }
    if (start == pos) {
      bad(origin, std::string("missing ") + field + " in header");
    }
    // Guard against absurd sizes before narrowing to uint64 arithmetic.
    if (pos - start > 18) bad(origin, std::string(field) + " is out of range");
    std::uint64_t value = 0;
    for (std::size_t i = start; i < pos; ++i) {
      value = value * 10 + static_cast<std::uint64_t>(text[i] - '0');
    }
    return value;
  }
};

}  // namespace

Image decode_ppm(const std::string& bytes, const std::string& origin) {
  if (bytes.size() < 2 || bytes[0] != 'P' || bytes[1] != '6') {
    bad(origin, "expected P6 binary PPM magic");
  }
  HeaderCursor cur{bytes, 2};
  cur.skip_filler();
  if (cur.pos == 2) bad(origin, "expected whitespace after magic");
  const std::uint64_t w = cur.read_uint(origin, "width");
  const std::uint64_t h = cur.read_uint(origin, "height");
  const std::uint64_t maxval = cur.read_uint(origin, "maxval");

  if (w == 0 || h == 0) bad(origin, "zero width or height");
  if (w > 100000 || h > 100000) bad(origin, "dimensions exceed supported limit");
  if (maxval != 255) {
    bad(origin, "maxval must be 255 (8-bit), got " + std::to_string(maxval));
  }

  // Exactly one whitespace byte separates the header from the raster. Tolerate
  // a CRLF pair for files written on Windows.
  if (cur.pos >= bytes.size() ||
      !is_space(static_cast<unsigned char>(bytes[cur.pos]))) {
    bad(origin, "expected whitespace before raster payload");
  }
  if (bytes[cur.pos] == '\r' && cur.pos + 1 < bytes.size() &&
      bytes[cur.pos + 1] == '\n') {
    cur.pos += 2;
  } else {
    ++cur.pos;
  }

  const std::uint64_t pixels = w * h;
  const std::uint64_t expected = pixels * 3;
  const std::uint64_t available = bytes.size() - cur.pos;
  if (available != expected) {
    bad(origin, "payload is " + std::to_string(available) + " bytes, expected " +
                    std::to_string(expected) +
                    " (exact raster required, no trailing data)");
  }

  Image image;
  image.width = static_cast<int>(w);
  image.height = static_cast<int>(h);
  image.rgb.resize(static_cast<std::size_t>(expected));
  for (std::size_t i = 0; i < image.rgb.size(); ++i) {
    image.rgb[i] = static_cast<std::uint8_t>(bytes[cur.pos + i]);
  }
  return image;
}

std::string encode_ppm(const Image& image) {
  if (image.width <= 0 || image.height <= 0) {
    throw Error(ExitCode::kRender, "ppm_invalid", "cannot encode empty image");
  }
  if (image.rgb.size() != image.pixel_count() * 3) {
    throw Error(ExitCode::kRender, "ppm_invalid",
                "pixel buffer does not match dimensions");
  }
  std::ostringstream out;
  out << "P6\n" << image.width << ' ' << image.height << "\n255\n";
  std::string header = out.str();
  header.reserve(header.size() + image.rgb.size());
  header.append(reinterpret_cast<const char*>(image.rgb.data()),
                image.rgb.size());
  return header;
}

Image read_ppm(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    throw Error(ExitCode::kUsage, "ppm_io",
                "cannot open input PPM: " + path);
  }
  std::ostringstream buf;
  buf << in.rdbuf();
  if (in.bad()) {
    throw Error(ExitCode::kUsage, "ppm_io", "failed reading PPM: " + path);
  }
  return decode_ppm(buf.str(), path);
}

void write_ppm(const std::string& path, const Image& image) {
  const std::string bytes = encode_ppm(image);
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) {
    throw Error(ExitCode::kRender, "ppm_io",
                "cannot open output PPM for writing: " + path);
  }
  out.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
  out.flush();
  if (!out) {
    throw Error(ExitCode::kRender, "ppm_io",
                "failed writing output PPM: " + path);
  }
}

}  // namespace lutgrade
