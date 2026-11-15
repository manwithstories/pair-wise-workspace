#include "types.h"

namespace sublay {

bool utf8_decode(const std::string& s, std::vector<UChar>& out, size_t& err_col) {
  out.clear();
  size_t i = 0;
  const size_t n = s.size();
  while (i < n) {
    unsigned char c = static_cast<unsigned char>(s[i]);
    uint32_t cp;
    size_t len;
    if (c < 0x80) { cp = c; len = 1; }
    else if ((c & 0xE0) == 0xC0) { cp = c & 0x1F; len = 2; }
    else if ((c & 0xF0) == 0xE0) { cp = c & 0x0F; len = 3; }
    else if ((c & 0xF8) == 0xF0) { cp = c & 0x07; len = 4; }
    else { err_col = i; return false; }
    if (i + len > n) { err_col = i; return false; }
    for (size_t k = 1; k < len; ++k) {
      unsigned char t = static_cast<unsigned char>(s[i + k]);
      if ((t & 0xC0) != 0x80) { err_col = i + k; return false; }
      cp = (cp << 6) | (t & 0x3F);
    }
    uint32_t min_cp = len == 1 ? 0x0 : len == 2 ? 0x80 : len == 3 ? 0x800 : 0x10000;
    if (cp < min_cp || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF)) {
      err_col = i;
      return false;
    }
    out.push_back({cp, i});
    i += len;
  }
  return true;
}

static bool in(uint32_t cp, uint32_t a, uint32_t b) { return cp >= a && cp <= b; }

bool is_combining(uint32_t cp) {
  return in(cp, 0x0300, 0x036F) || in(cp, 0x0483, 0x0489) ||
         in(cp, 0x0591, 0x05BD) || in(cp, 0x0610, 0x061A) ||
         in(cp, 0x064B, 0x065F) || in(cp, 0x0E31, 0x0E3A) ||
         in(cp, 0x0E47, 0x0E4E) || in(cp, 0x1AB0, 0x1AFF) ||
         in(cp, 0x1DC0, 0x1DFF) || in(cp, 0x20D0, 0x20FF) ||
         in(cp, 0xFE20, 0xFE2F);
}

bool is_variation_selector(uint32_t cp) {
  return in(cp, 0xFE00, 0xFE0F) || in(cp, 0xE0100, 0xE01EF);
}

bool is_zwj(uint32_t cp) { return cp == 0x200D || cp == 0x200C; }

bool is_rtl_cp(uint32_t cp) {
  return in(cp, 0x0590, 0x05FF) || in(cp, 0x0600, 0x06FF) ||
         in(cp, 0x0750, 0x077F) || in(cp, 0x08A0, 0x08FF) ||
         in(cp, 0xFB50, 0xFDFF) || in(cp, 0xFE70, 0xFEFF);
}

bool is_cjk_cp(uint32_t cp) {
  return in(cp, 0x2E80, 0x33FF) || in(cp, 0x3400, 0x4DBF) ||
         in(cp, 0x4E00, 0x9FFF) || in(cp, 0xF900, 0xFAFF) ||
         in(cp, 0xFF00, 0xFFEF) || in(cp, 0x20000, 0x2FA1F);
}

bool is_space_cp(uint32_t cp) {
  return cp == 0x20 || cp == 0x09 || cp == 0x3000 || cp == 0x0A;
}

bool kinsoku_no_line_start(uint32_t cp) {
  switch (cp) {
    case 0x3001: case 0x3002: case 0xFF0C: case 0xFF0E: case 0xFF01:
    case 0xFF1F: case 0xFF1A: case 0xFF1B: case 0xFF09: case 0x3011:
    case 0x300D: case 0x300F: case 0xFF5D: case 0x2026: case 0x2025:
    case 0x3005: case 0x30FC: case 0xFF3D: case 0x002C: case 0x002E:
    case 0x0021: case 0x003F: case 0x003A: case 0x003B: case 0x0029:
    case 0x005D: case 0x007D: case 0x060C: case 0x061B: case 0x061F:
      return true;
    default:
      return false;
  }
}

bool kinsoku_no_line_end(uint32_t cp) {
  switch (cp) {
    case 0xFF08: case 0x3010: case 0x300C: case 0x300E: case 0xFF5B:
    case 0xFF3B: case 0x0028: case 0x005B: case 0x007B: case 0x00AB:
    case 0x201C: case 0x2018:
      return true;
    default:
      return false;
  }
}

bool is_url_char(uint32_t cp) {
  if (cp > 0x7E) return false;
  unsigned char c = static_cast<unsigned char>(cp);
  if (c <= 0x20) return false;
  switch (c) {
    case '<': case '>': case '"': case '\'': case '`':
      return false;
    default:
      return true;
  }
}

std::vector<Cluster> make_clusters(const std::vector<UChar>& cps, size_t text_len) {
  std::vector<Cluster> out;
  size_t i = 0;
  const size_t n = cps.size();
  while (i < n) {
    Cluster cl;
    cl.byte_off = cps[i].byte_off;
    cl.first_cp = cps[i].cp;
    cl.rtl = is_rtl_cp(cps[i].cp);
    size_t j = i + 1;
    bool join_next = is_zwj(cps[i].cp);
    bool only_marks = is_combining(cps[i].cp) ||
                      is_variation_selector(cps[i].cp) || is_zwj(cps[i].cp);
    while (j < n) {
      uint32_t cp = cps[j].cp;
      if (is_combining(cp) || is_variation_selector(cp)) {
        ++j;
        join_next = false;
        continue;
      }
      if (is_zwj(cp)) { ++j; join_next = true; continue; }
      if (join_next) {  // ZWJ sequence: absorb following base + its marks
        ++j;
        join_next = false;
        only_marks = false;
        continue;
      }
      break;
    }
    size_t end_off = j < n ? cps[j].byte_off : text_len;
    cl.byte_len = end_off - cl.byte_off;
    cl.zero_width = only_marks;
    out.push_back(cl);
    i = j;
  }
  return out;
}

}  // namespace sublay
