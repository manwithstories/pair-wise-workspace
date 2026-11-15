#include "ttf_synth.hpp"

#include <algorithm>
#include <array>
#include <cstdint>
#include <fstream>
#include <limits>
#include <map>
#include <string>
#include <tuple>
#include <vector>

namespace smx {
namespace {

using Bytes = std::vector<std::uint8_t>;
using Table = std::pair<std::string, Bytes>;

constexpr std::uint16_t kUnitsPerEm = 1000;
constexpr std::int16_t kAscender = 880;
constexpr std::int16_t kDescender = -120;
constexpr std::int16_t kLineGap = 40;

void put16(Bytes& b, std::uint16_t v) {
  b.push_back(static_cast<std::uint8_t>(v >> 8));
  b.push_back(static_cast<std::uint8_t>(v));
}
void put32(Bytes& b, std::uint32_t v) {
  b.push_back(static_cast<std::uint8_t>(v >> 24));
  b.push_back(static_cast<std::uint8_t>(v >> 16));
  b.push_back(static_cast<std::uint8_t>(v >> 8));
  b.push_back(static_cast<std::uint8_t>(v));
}
void puti16(Bytes& b, std::int16_t v) { put16(b, static_cast<std::uint16_t>(v)); }
void patch16(Bytes& b, std::size_t at, std::uint16_t v) {
  b[at] = static_cast<std::uint8_t>(v >> 8);
  b[at + 1] = static_cast<std::uint8_t>(v);
}
void patch32(Bytes& b, std::size_t at, std::uint32_t v) {
  b[at] = static_cast<std::uint8_t>(v >> 24);
  b[at + 1] = static_cast<std::uint8_t>(v >> 16);
  b[at + 2] = static_cast<std::uint8_t>(v >> 8);
  b[at + 3] = static_cast<std::uint8_t>(v);
}
std::uint32_t checksum(const Bytes& bytes) {
  std::uint32_t sum = 0;
  for (std::size_t i = 0; i < bytes.size(); i += 4) {
    std::uint32_t word = 0;
    for (std::size_t j = 0; j < 4; ++j) {
      word <<= 8;
      if (i + j < bytes.size()) word |= bytes[i + j];
    }
    sum += word;
  }
  return sum;
}
std::uint64_t mix(std::uint64_t x) {
  x += 0x9e3779b97f4a7c15ULL;
  x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9ULL;
  x = (x ^ (x >> 27)) * 0x94d049bb133111ebULL;
  return x ^ (x >> 31);
}
std::uint16_t log2floor(std::uint32_t v) {
  std::uint16_t n = 0;
  while (v > 1) { v >>= 1; ++n; }
  return n;
}

struct Glyph {
  std::uint32_t cp = 0;
  std::uint16_t advance = 0;
  std::int16_t xmin = 0, ymin = 0, xmax = 0, ymax = 0;
  bool empty = true;
};

std::vector<std::uint32_t> codepoints() {
  std::vector<std::uint32_t> cps;
  for (std::uint32_t cp = 0x20; cp <= 0x7e; ++cp) cps.push_back(cp);
  const std::uint32_t extras[] = {
      0x00a0, 0x00ad, 0x0301, 0x200b, 0x200c, 0x200d, 0x2010, 0x2011,
      0x2026, 0x3001, 0x3002, 0x300c, 0x300d, 0x300e, 0x300f, 0x3010,
      0x3011, 0xff01, 0xff08, 0xff09, 0xff0c, 0xff1a, 0xff1b, 0xff1f,
      0xfffd};
  cps.insert(cps.end(), std::begin(extras), std::end(extras));
  const std::u32string common_cjk =
      U"\u4e2d\u6587\u5b57\u5e55\u6d4b\u8bd5\u907f\u5934\u5c3e\u82f1\u8fd0\u884c\u65f6\u52a8\u6001\u6392\u7248\u9884\u68c0\u91cf"
      U"\u5305\u56f4\u76d2\u7701\u7565\u53f7\u6362\u4f4d\u7f6e\u96f6\u5bbd\u65ad\u975e\u6cd5\u8282\u6837\u5f0f\u5206\u6bb5\u6279"
      U"\u5904\u7406\u6ea2\u51fa\u6761\u76ee\u5f62\u5ea6\u4f59\u672b\u5b64\u7acb\u5168\u89d2\u6807\u70b9\u786c\u4f18\u5148\u8d2a"
      U"\u5fc3\u7b97\u89c4\u5212\u6700\u5c0f\u5316\u5e76\u7a33\u5b9a\u6458\u8981\u540c\u8f93\u5165\u4f4e\u5185\u5b58\u53ef\u590d"
      U"\u73b0\u4e5d\u8bed\u79cd\u6df7\u5408\u7ec4\u9644\u52a0\u7b26\u7b2c\u4e00\u4e8c\u4fdd\u6301\u7ea7\u7684\u662f\u5728\u4e0d"
      U"\u4e86\u6709\u548c\u4eba\u8fd9\u5927\u4e3a\u4e0a\u4e2a\u56fd\u6211\u4ee5\u4ed6\u6765\u7528\u4eec\u751f\u5230\u4f5c\u5730"
      U"\u4e8e\u5c31\u5bf9\u6210\u4f1a\u4e3b\u53d1\u5e74\u5de5\u4e5f\u80fd\u4e0b\u8fc7\u5b50\u8bf4\u4ea7\u9762\u800c\u65b9\u540e"
      U"\u591a\u5b66\u6240\u6c11\u5f97\u7ecf\u5341\u4e09\u4e4b\u8fdb\u7740\u7b49\u90e8\u5bb6\u7535\u529b\u91cc\u5982\u6c34\u9ad8"
      U"\u81ea\u8d77\u7269\u5b9e\u90fd\u4e24\u4f53\u5236\u673a\u5f53\u4f7f\u4ece\u4e1a\u672c\u53bb\u628a\u6027\u597d\u5e94\u5f00"
      U"\u5b83\u8fd8\u56e0\u7531\u5176\u4e9b\u7136\u524d\u5916\u5929\u653f\u56db\u65e5\u90a3\u793e\u4e49\u4e8b\u5e73\u76f8\u8868";
  for (char32_t cp : common_cjk) cps.push_back(static_cast<std::uint32_t>(cp));
  std::sort(cps.begin(), cps.end());
  cps.erase(std::unique(cps.begin(), cps.end()), cps.end());
  return cps;
}

bool is_cjk(std::uint32_t cp) { return cp >= 0x4e00 && cp <= 0x9fff; }
bool is_fullwidth(std::uint32_t cp) {
  return (cp >= 0x3000 && cp <= 0x303f) || (cp >= 0xff01 && cp <= 0xff5e) ||
         cp == 0x2026;
}
bool is_zero_width(std::uint32_t cp) {
  return cp == 0x200b || cp == 0x200c || cp == 0x200d || cp == 0xfeff ||
         (cp >= 0xfe00 && cp <= 0xfe0f) ||
         (cp >= 0x0300 && cp <= 0x036f) ||
         (cp >= 0x1ab0 && cp <= 0x1aff) ||
         (cp >= 0x1dc0 && cp <= 0x1dff) ||
         (cp >= 0x20d0 && cp <= 0x20ff) ||
         (cp >= 0xfe20 && cp <= 0xfe2f);
}

std::uint16_t advance_for(std::uint32_t cp, std::uint64_t seed) {
  if (is_zero_width(cp)) return 0;
  if (is_cjk(cp) || is_fullwidth(cp)) return 1000;
  if (cp == 0x20 || cp == 0x00a0) return 300;
  int base = 560;
  if (cp == 'i' || cp == 'l' || cp == 'I' || cp == '!' || cp == '|') base = 300;
  else if (cp == 'm' || cp == 'w' || cp == 'M' || cp == 'W') base = 780;
  else if ((cp >= '0' && cp <= '9') || (cp >= 'A' && cp <= 'Z') ||
           (cp >= 'a' && cp <= 'z')) base = 580;
  else base = 440;
  const int jitter = static_cast<int>((mix(seed ^ cp) >> 12) % 9) - 4;
  return static_cast<std::uint16_t>(base + jitter);
}

Glyph make_glyph(std::uint32_t cp, std::uint16_t advance, std::uint64_t seed) {
  Glyph g;
  g.cp = cp;
  g.advance = advance;
  if (cp == 0 || cp == 0xfffd) {
    g.empty = false; g.xmin = 70; g.ymin = 0; g.xmax = 930; g.ymax = 700;
  } else if (is_zero_width(cp)) {
    if (cp >= 0x0300) {
      g.empty = false; g.xmin = 250; g.ymin = 650; g.xmax = 500; g.ymax = 840;
    }
  } else if (is_cjk(cp) || is_fullwidth(cp)) {
    const int inset = static_cast<int>((mix(seed ^ cp) >> 20) % 36);
    g.empty = false; g.xmin = static_cast<std::int16_t>(35 + inset);
    g.ymin = 0; g.xmax = static_cast<std::int16_t>(965 - inset); g.ymax = 735;
  } else if (cp == 0x20 || cp == 0x00a0) {
  } else {
    const int side = cp == '.' || cp == ',' || cp == ':' || cp == ';' ? 145 : 65;
    g.empty = false; g.xmin = static_cast<std::int16_t>(side); g.ymin = 0;
    g.xmax = static_cast<std::int16_t>(std::max<int>(side + 20, advance - side));
    g.ymax = (cp == '.' || cp == ',') ? 180 : 690;
  }
  return g;
}

Bytes glyph_data(const Glyph& g) {
  Bytes b;
  if (g.empty) return b;
  puti16(b, 1);
  puti16(b, g.xmin); puti16(b, g.ymin); puti16(b, g.xmax); puti16(b, g.ymax);
  put16(b, 3);
  put16(b, 0);
  for (int i = 0; i < 4; ++i) b.push_back(0x01);
  const std::int16_t xs[4] = {g.xmin, g.xmax, g.xmax, g.xmin};
  const std::int16_t ys[4] = {g.ymin, g.ymin, g.ymax, g.ymax};
  std::int16_t px = 0, py = 0;
  for (int i = 0; i < 4; ++i) { puti16(b, static_cast<std::int16_t>(xs[i] - px)); px = xs[i]; }
  for (int i = 0; i < 4; ++i) { puti16(b, static_cast<std::int16_t>(ys[i] - py)); py = ys[i]; }
  if (b.size() & 1U) b.push_back(0);
  return b;
}

Bytes cmap_table(const std::vector<Glyph>& glyphs) {
  std::vector<std::pair<std::uint32_t, std::uint16_t>> map;
  for (std::size_t i = 0; i < glyphs.size() && i <= 0xffff; ++i)
    map.emplace_back(glyphs[i].cp, static_cast<std::uint16_t>(i));
  std::sort(map.begin(), map.end());
  map.erase(std::unique(map.begin(), map.end(),
                        [](const auto& a, const auto& b) { return a.first == b.first; }),
            map.end());
  const std::uint16_t seg_count = static_cast<std::uint16_t>(map.size() + 1);
  const std::uint16_t seg_x2 = static_cast<std::uint16_t>(seg_count * 2);
  const std::uint16_t power = static_cast<std::uint16_t>(1U << log2floor(seg_count));
  const std::uint16_t search = static_cast<std::uint16_t>(power * 2);
  const std::uint16_t selector = log2floor(seg_count);
  const std::uint16_t range_shift = static_cast<std::uint16_t>(seg_x2 - search);
  Bytes sub;
  put16(sub, 4);
  put16(sub, 0);
  put16(sub, 0);
  put16(sub, seg_x2); put16(sub, search); put16(sub, selector); put16(sub, range_shift);
  for (const auto& e : map) put16(sub, static_cast<std::uint16_t>(e.first));
  put16(sub, 0xffff);
  put16(sub, 0);
  for (const auto& e : map) put16(sub, static_cast<std::uint16_t>(e.first));
  put16(sub, 0xffff);
  for (const auto& e : map)
    put16(sub, static_cast<std::uint16_t>(e.second - e.first));
  put16(sub, 1);
  for (std::size_t i = 0; i < map.size(); ++i) put16(sub, 0);
  put16(sub, 0);
  patch16(sub, 2, static_cast<std::uint16_t>(sub.size()));
  Bytes out;
  put16(out, 0); put16(out, 1);
  put16(out, 3); put16(out, 1); put32(out, 12);
  out.insert(out.end(), sub.begin(), sub.end());
  return out;
}

Bytes kern_table(const std::vector<Glyph>& glyphs) {
  std::map<std::uint32_t, std::uint16_t> gid;
  for (std::size_t i = 0; i < glyphs.size() && i <= 0xffff; ++i)
    gid[glyphs[i].cp] = static_cast<std::uint16_t>(i);
  struct Pair { std::uint16_t left, right; std::int16_t value; };
  std::vector<Pair> pairs;
  const std::uint32_t lefts[] = {'A', 'V', 'W', 'T', 'Y', 'L', 'P', 'F'};
  const std::uint32_t rights[] = {'V', 'A', 'a', 'o', 'e', 'T', 'Y', '.'};
  for (std::uint32_t l : lefts) {
    auto li = gid.find(l);
    if (li == gid.end()) continue;
    for (std::uint32_t r : rights) {
      auto ri = gid.find(r);
      if (ri == gid.end()) continue;
      if ((l == 'A' && r == 'V') || (l == 'V' && r == 'A') ||
          (l == 'W' && r == 'a') || (l == 'T' && r == 'o') ||
          (l == 'Y' && r == 'e') || (l == 'L' && r == 'T') ||
          (l == 'P' && r == '.') || (l == 'F' && r == 'A'))
        pairs.push_back({li->second, ri->second, -55});
    }
  }
  if (pairs.empty()) {
    Bytes empty;
    put16(empty, 0); put16(empty, 0);
    return empty;
  }
  std::sort(pairs.begin(), pairs.end(), [](const Pair& a, const Pair& b) {
    return (static_cast<std::uint32_t>(a.left) << 16 | a.right) <
           (static_cast<std::uint32_t>(b.left) << 16 | b.right);
  });
  pairs.erase(std::unique(pairs.begin(), pairs.end(), [](const Pair& a, const Pair& b) {
    return a.left == b.left && a.right == b.right;
  }), pairs.end());
  const std::uint16_t n = static_cast<std::uint16_t>(pairs.size());
  const std::uint16_t power = static_cast<std::uint16_t>(1U << log2floor(n));
  const std::uint16_t search = static_cast<std::uint16_t>(power * 6);
  const std::uint16_t selector = log2floor(n);
  const std::uint16_t range = static_cast<std::uint16_t>(n * 6 - search);
  Bytes sub;
  put16(sub, 0);
  put16(sub, static_cast<std::uint16_t>(14 + n * 6));
  put16(sub, 1);
  put16(sub, n); put16(sub, search); put16(sub, selector); put16(sub, range);
  for (const auto& pair : pairs) {
    put16(sub, pair.left); put16(sub, pair.right); puti16(sub, pair.value);
  }
  Bytes out;
  put16(out, 0); put16(out, 1);
  out.insert(out.end(), sub.begin(), sub.end());
  return out;
}

Bytes name_table(std::uint64_t seed) {
  const std::string family = "SMX Synth " + std::to_string(seed);
  const std::string sub = "Regular";
  const std::string full = family + " Regular";
  const std::string ps = "SMXSynth-" + std::to_string(seed);
  struct Entry { std::uint16_t id; const std::string* text; };
  const Entry entries[] = {{1, &family}, {2, &sub}, {4, &full}, {6, &ps}};
  Bytes strings;
  for (const auto& e : entries)
    for (unsigned char c : *e.text) { strings.push_back(0); strings.push_back(c); }
  Bytes b;
  put16(b, 0); put16(b, static_cast<std::uint16_t>(std::size(entries)));
  put16(b, static_cast<std::uint16_t>(6 + std::size(entries) * 12));
  std::uint16_t offset = 0;
  for (const auto& e : entries) {
    put16(b, 3); put16(b, 1); put16(b, 0x0409); put16(b, e.id);
    put16(b, static_cast<std::uint16_t>(e.text->size() * 2));
    put16(b, offset); offset = static_cast<std::uint16_t>(offset + e.text->size() * 2);
  }
  b.insert(b.end(), strings.begin(), strings.end());
  return b;
}

Bytes os2_table(const std::vector<Glyph>& glyphs) {
  Bytes b;
  std::uint32_t sum = 0;
  for (const auto& g : glyphs) sum += g.advance;
  put16(b, 4);
  puti16(b, static_cast<std::int16_t>(sum / std::max<std::size_t>(1, glyphs.size())));
  put16(b, 400); put16(b, 5); put16(b, 0);
  for (int i = 0; i < 10; ++i) b.push_back(0);
  puti16(b, 650); puti16(b, 600); puti16(b, 0); puti16(b, 75);
  puti16(b, 650); puti16(b, 600); puti16(b, 0); puti16(b, 350);
  puti16(b, 50); put16(b, 0);
  for (int i = 0; i < 10; ++i) b.push_back(0);
  for (int i = 0; i < 4; ++i) put32(b, 0);
  for (char c : std::string("SMX ")) b.push_back(static_cast<std::uint8_t>(c));
  put16(b, 0x0040);
  std::uint32_t first_cp = 0x20, last_cp = 0x20;
  for (const auto& g : glyphs) {
    if (g.cp > 0) { first_cp = std::min(first_cp, g.cp); last_cp = std::max(last_cp, g.cp); }
  }
  put16(b, static_cast<std::uint16_t>(std::min<std::uint32_t>(first_cp, 0xffff)));
  put16(b, static_cast<std::uint16_t>(std::min<std::uint32_t>(last_cp, 0xffff)));
  puti16(b, kAscender); puti16(b, kDescender); puti16(b, kLineGap);
  put16(b, 1000); put16(b, 200);
  put32(b, 1); put32(b, 0);
  puti16(b, 500); puti16(b, 700);
  put16(b, 0); put16(b, 0x20); put16(b, 1);
  return b;
}

Bytes head_table(std::uint16_t min_x, std::uint16_t min_y,
                 std::uint16_t max_x, std::uint16_t max_y) {
  Bytes b;
  put32(b, 0x00010000); put32(b, 0x00010000); put32(b, 0); put32(b, 0x5f0f3cf5);
  put16(b, 0x0003); put16(b, kUnitsPerEm);
  put32(b, 0); put32(b, 0); put32(b, 0); put32(b, 0);
  puti16(b, static_cast<std::int16_t>(min_x)); puti16(b, static_cast<std::int16_t>(min_y));
  puti16(b, static_cast<std::int16_t>(max_x)); puti16(b, static_cast<std::int16_t>(max_y));
  put16(b, 0); put16(b, 8); puti16(b, 2); puti16(b, 1); puti16(b, 0);
  return b;
}

Bytes hhea_table(std::uint16_t num_hmetrics, std::uint16_t advance_max) {
  Bytes b;
  put32(b, 0x00010000); puti16(b, kAscender); puti16(b, kDescender); puti16(b, kLineGap);
  put16(b, advance_max); puti16(b, 0); puti16(b, 0); puti16(b, advance_max);
  puti16(b, 1); puti16(b, 0); puti16(b, 0);
  for (int i = 0; i < 4; ++i) puti16(b, 0);
  puti16(b, 0); put16(b, num_hmetrics);
  return b;
}

std::uint32_t table_checksum(const Bytes& b) { return checksum(b); }

}  // namespace

SynthFontResult synth_font(std::uint64_t seed) {
  SynthFontResult result;
  const auto cps = codepoints();
  if (cps.empty()) { result.error = "font has no codepoints"; return result; }
  std::vector<Glyph> glyphs;
  Glyph notdef; notdef.cp = 0; notdef.advance = 600;
  notdef.empty = false; notdef.xmin = 70; notdef.ymin = 0; notdef.xmax = 530; notdef.ymax = 700;
  glyphs.push_back(notdef);
  for (std::uint32_t cp : cps) glyphs.push_back(make_glyph(cp, advance_for(cp, seed), seed));

  Bytes glyf;
  std::vector<std::uint32_t> loca;
  for (const auto& g : glyphs) {
    loca.push_back(static_cast<std::uint32_t>(glyf.size()));
    Bytes data = glyph_data(g);
    glyf.insert(glyf.end(), data.begin(), data.end());
  }
  loca.push_back(static_cast<std::uint32_t>(glyf.size()));
  if (glyf.size() & 1U) glyf.push_back(0);

  Bytes loca_data;
  for (std::uint32_t off : loca) put32(loca_data, off);
  Bytes hmtx;
  std::uint16_t adv_max = 0;
  for (const auto& g : glyphs) {
    put16(hmtx, g.advance); puti16(hmtx, g.xmin); adv_max = std::max(adv_max, g.advance);
  }
  Bytes maxp;
  put32(maxp, 0x00010000); put16(maxp, static_cast<std::uint16_t>(glyphs.size()));
  put16(maxp, 4); put16(maxp, 1); put16(maxp, 0); put16(maxp, 0);
  for (int i = 0; i < 10; ++i) put16(maxp, 0);
  Bytes post;
  put32(post, 0x00030000); put32(post, 0); puti16(post, -100); puti16(post, 50);
  put32(post, 0); put32(post, 0); put32(post, 0); put32(post, 0);

  std::vector<Table> tables;
  tables.emplace_back("OS/2", os2_table(glyphs));
  tables.emplace_back("cmap", cmap_table(glyphs));
  tables.emplace_back("glyf", std::move(glyf));
  tables.emplace_back("head", head_table(0, 0, 1000, 880));
  tables.emplace_back("hhea", hhea_table(static_cast<std::uint16_t>(glyphs.size()), adv_max));
  tables.emplace_back("hmtx", std::move(hmtx));
  tables.emplace_back("kern", kern_table(glyphs));
  tables.emplace_back("loca", std::move(loca_data));
  tables.emplace_back("maxp", std::move(maxp));
  tables.emplace_back("name", name_table(seed));
  tables.emplace_back("post", std::move(post));

  const std::uint16_t n = static_cast<std::uint16_t>(tables.size());
  const std::uint16_t power = static_cast<std::uint16_t>(1U << log2floor(n));
  const std::uint16_t search = static_cast<std::uint16_t>(power * 16);
  const std::uint16_t selector = log2floor(n);
  const std::uint16_t range = static_cast<std::uint16_t>(n * 16 - search);
  Bytes font;
  put32(font, 0x00010000); put16(font, n); put16(font, search);
  put16(font, selector); put16(font, range);
  const std::size_t directory_at = font.size();
  font.resize(font.size() + static_cast<std::size_t>(n) * 16);
  std::size_t offset = font.size();
  std::vector<std::size_t> head_record;
  for (std::size_t i = 0; i < tables.size(); ++i) {
    Bytes data = tables[i].second;
    const std::uint32_t cs = table_checksum(data);
    const std::size_t rec = directory_at + i * 16;
    for (int j = 0; j < 4; ++j) font[rec + j] = static_cast<std::uint8_t>(tables[i].first[j]);
    patch32(font, rec + 4, cs);
    patch32(font, rec + 8, static_cast<std::uint32_t>(offset));
    patch32(font, rec + 12, static_cast<std::uint32_t>(data.size()));
    if (tables[i].first == "head") head_record = {rec, offset};
    font.insert(font.end(), data.begin(), data.end());
    while (font.size() & 3U) font.push_back(0);
    offset = font.size();
  }
  const std::uint32_t whole = checksum(font);
  const std::uint32_t adjustment = 0xb1b0afbaU - whole;
  if (!head_record.empty()) {
    const std::size_t head_off = head_record[1] + 8;
    if (head_off + 4 <= font.size()) patch32(font, head_off, adjustment);
  }
  result.bytes = std::move(font);
  result.codepoints = cps;
  result.ok = true;
  return result;
}

bool write_synth_font(const std::string& path, std::uint64_t seed, std::string& error) {
  auto r = synth_font(seed);
  if (!r.ok) { error = r.error; return false; }
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) { error = "cannot open output font: " + path; return false; }
  out.write(reinterpret_cast<const char*>(r.bytes.data()),
            static_cast<std::streamsize>(r.bytes.size()));
  if (!out) { error = "cannot write output font: " + path; return false; }
  return true;
}
}  // namespace smx
