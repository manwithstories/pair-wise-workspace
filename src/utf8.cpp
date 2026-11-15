#include "utf8.hpp"

#include <algorithm>

namespace smx {
namespace {
bool continuation(unsigned char c) { return (c & 0xc0U) == 0x80U; }

std::size_t invalid_prefix_length(std::string_view input, std::size_t at,
                                  unsigned char lead) {
  auto cont = [&](std::size_t index) {
    return index < input.size() && continuation(static_cast<unsigned char>(input[index]));
  };
  if (lead >= 0xc2 && lead <= 0xdf)
    return cont(at + 1) ? 2 : 1;
  if (lead >= 0xe0 && lead <= 0xef) {
    if (!cont(at + 1)) return 1;
    const auto c1 = static_cast<unsigned char>(input[at + 1]);
    if ((lead == 0xe0 && c1 < 0xa0) || (lead == 0xed && c1 > 0x9f)) return 1;
    return cont(at + 2) ? 3 : 2;
  }
  if (lead >= 0xf0 && lead <= 0xf4) {
    if (!cont(at + 1)) return 1;
    const auto c1 = static_cast<unsigned char>(input[at + 1]);
    if ((lead == 0xf0 && c1 < 0x90) || (lead == 0xf4 && c1 > 0x8f)) return 1;
    if (!cont(at + 2)) return 2;
    return cont(at + 3) ? 4 : 3;
  }
  return 1;
}

void add_replacement(Utf8DecodeResult& out, std::size_t at, std::size_t len) {
  Cluster c;
  c.cps.push_back(0xfffd);
  c.first_cp = 0xfffd;
  c.byte_start = at;
  c.byte_end = at + len;
  out.text.push_back(0xfffd);
  out.clusters.push_back(std::move(c));
  ++out.invalid_sequences;
}
}  // namespace

bool is_combining_mark(std::uint32_t cp) {
  return (cp >= 0x0300 && cp <= 0x036f) || (cp >= 0x1ab0 && cp <= 0x1aff) ||
         (cp >= 0x1dc0 && cp <= 0x1dff) || (cp >= 0x20d0 && cp <= 0x20ff) ||
         (cp >= 0xfe20 && cp <= 0xfe2f);
}

bool is_zero_width_cp(std::uint32_t cp) {
  return cp == 0x200b || cp == 0x200c || cp == 0x200d || cp == 0xfeff ||
         (cp >= 0xfe00 && cp <= 0xfe0f) || is_combining_mark(cp) || cp == 0x2060;
}

bool is_cjk_cp(std::uint32_t cp) {
  return (cp >= 0x2e80 && cp <= 0x9fff) || (cp >= 0xf900 && cp <= 0xfaff) ||
         (cp >= 0x20000 && cp <= 0x2fa1f);
}

std::string utf8_encode(std::uint32_t cp) {
  std::string out;
  if (cp <= 0x7f) {
    out.push_back(static_cast<char>(cp));
  } else if (cp <= 0x7ff) {
    out.push_back(static_cast<char>(0xc0 | (cp >> 6)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3f)));
  } else if (cp <= 0xffff) {
    out.push_back(static_cast<char>(0xe0 | (cp >> 12)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3f)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3f)));
  } else {
    out.push_back(static_cast<char>(0xf0 | (cp >> 18)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3f)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3f)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3f)));
  }
  return out;
}

std::string utf8_encode(const std::u32string& text) {
  std::string out;
  for (std::uint32_t cp : text) out += utf8_encode(cp);
  return out;
}

Utf8DecodeResult decode_utf8(std::string_view input) {
  Utf8DecodeResult out;
  out.text.reserve(input.size());
  std::size_t i = 0;
  while (i < input.size()) {
    const auto c = static_cast<unsigned char>(input[i]);
    std::uint32_t cp = 0;
    std::size_t len = 0;
    if (c < 0x80) {
      cp = c;
      len = 1;
    } else if (c >= 0xc2 && c <= 0xdf) {
      if (i + 1 < input.size() && continuation(static_cast<unsigned char>(input[i + 1]))) {
        cp = ((c & 0x1fU) << 6) | (static_cast<unsigned char>(input[i + 1]) & 0x3fU);
        len = 2;
      }
    } else if (c >= 0xe0 && c <= 0xef) {
      if (i + 2 < input.size() && continuation(static_cast<unsigned char>(input[i + 1])) &&
          continuation(static_cast<unsigned char>(input[i + 2]))) {
        const auto c1 = static_cast<unsigned char>(input[i + 1]);
        const auto c2 = static_cast<unsigned char>(input[i + 2]);
        cp = ((c & 0x0fU) << 12) | ((c1 & 0x3fU) << 6) | (c2 & 0x3fU);
        if (cp >= 0x800 && !(cp >= 0xd800 && cp <= 0xdfff)) len = 3;
      }
    } else if (c >= 0xf0 && c <= 0xf4) {
      if (i + 3 < input.size() && continuation(static_cast<unsigned char>(input[i + 1])) &&
          continuation(static_cast<unsigned char>(input[i + 2])) &&
          continuation(static_cast<unsigned char>(input[i + 3]))) {
        cp = ((c & 0x07U) << 18) |
             ((static_cast<unsigned char>(input[i + 1]) & 0x3fU) << 12) |
             ((static_cast<unsigned char>(input[i + 2]) & 0x3fU) << 6) |
             (static_cast<unsigned char>(input[i + 3]) & 0x3fU);
        if (cp >= 0x10000 && cp <= 0x10ffff) len = 4;
      }
    }
    if (len == 0) {
      const std::size_t bad = invalid_prefix_length(input, i, c);
      add_replacement(out, i, bad);
      i += bad;
      continue;
    }
    if (is_combining_mark(cp) && !out.clusters.empty() && out.clusters.back().first_cp != '\n' &&
        out.clusters.back().first_cp != '\r') {
      out.clusters.back().cps.push_back(cp);
      out.clusters.back().byte_end = i + len;
    } else {
      Cluster cluster;
      cluster.cps.push_back(cp);
      cluster.first_cp = cp;
      cluster.byte_start = i;
      cluster.byte_end = i + len;
      out.clusters.push_back(std::move(cluster));
    }
    out.text.push_back(cp);
    i += len;
  }
  return out;
}
}  // namespace smx
