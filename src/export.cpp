#include "types.h"

#include <fstream>
#include <sstream>

namespace sublay {

static std::string esc(const std::string& s) {
  std::string o;
  for (unsigned char c : s) {
    switch (c) {
      case '"': o += "\\\""; break;
      case '\\': o += "\\\\"; break;
      case '\n': o += "\\n"; break;
      case '\r': o += "\\r"; break;
      case '\t': o += "\\t"; break;
      default:
        if (c < 0x20) {
          char b[8];
          snprintf(b, sizeof b, "\\u%04x", c);
          o += b;
        } else {
          o.push_back(static_cast<char>(c));
        }
    }
  }
  return o;
}

static std::string ind(int n) { return std::string(static_cast<size_t>(n), ' '); }

static std::string line_box_json(const LineBox& b) {
  std::ostringstream o;
  o << "{\"x_min\":" << b.x_min << ",\"y_min\":" << b.y_min
    << ",\"x_max\":" << b.x_max << ",\"y_max\":" << b.y_max << "}";
  return o.str();
}

static std::string breaks_json(const std::vector<size_t>& br) {
  std::ostringstream o;
  o << "[";
  for (size_t i = 0; i < br.size(); ++i) {
    if (i) o << ",";
    o << br[i];
  }
  o << "]";
  return o.str();
}

static std::string rules_json(const std::vector<std::string>& rules) {
  std::ostringstream o;
  o << "[";
  for (size_t i = 0; i < rules.size(); ++i) {
    if (i) o << ",";
    o << "\"" << esc(rules[i]) << "\"";
  }
  o << "]";
  return o.str();
}

std::string cue_metrics_json(const CueMetrics& m, int indent) {
  std::ostringstream o;
  std::string p = ind(indent), p2 = ind(indent + 2);
  o << "{\n";
  o << p2 << "\"cue_id\":\"" << esc(m.cue_id) << "\",\n";
  o << p2 << "\"line_count\":" << m.line_count << ",\n";
  o << p2 << "\"bbox\":" << line_box_json(m.bbox) << ",\n";
  o << p2 << "\"baseline\":" << m.bbox.baseline << ",\n";
  o << p2 << "\"breaks\":" << breaks_json(m.breaks) << ",\n";
  o << p2 << "\"overflow\":" << (m.overflow ? "true" : "false") << ",\n";
  o << p2 << "\"overflow_px\":" << m.overflow_px << ",\n";
  o << p2 << "\"missing_glyph\":" << (m.missing_glyph ? "true" : "false") << ",\n";
  o << p2 << "\"glyph_count\":" << m.glyph_count << ",\n";
  o << p2 << "\"rules\":" << rules_json(m.rules) << ",\n";
  o << p2 << "\"lines\":[";
  for (size_t i = 0; i < m.lines.size(); ++i) {
    const LineBox& lb = m.lines[i];
    if (i) o << ",";
    o << "\n" << p2 << "  {\"start_byte\":" << lb.start_byte
      << ",\"end_byte\":" << lb.end_byte << ",\"width\":" << lb.width
      << ",\"baseline\":" << lb.baseline << ",\"bbox\":" << line_box_json(lb)
      << "}";
  }
  if (!m.lines.empty()) o << "\n" << p2;
  o << "]\n" << p << "}";
  return o.str();
}

std::string summary_json(const MeasureOutput& out, const LayoutParams& params,
                         const std::vector<std::string>& font_paths) {
  std::ostringstream o;
  o << "{\n";
  o << "  \"version\":1,\n";
  o << "  \"units\":\"26.6\",\n";
  o << "  \"params\":{\"size\":" << params.size_px
    << ",\"width\":" << params.width << ",\"safe\":" << params.safe
    << ",\"max_lines\":" << params.max_lines << "},\n";
  o << "  \"fonts\":[";
  for (size_t i = 0; i < font_paths.size(); ++i) {
    if (i) o << ",";
    o << "\"" << esc(font_paths[i]) << "\"";
  }
  o << "],\n";
  o << "  \"stats\":{\"cues\":" << out.stats.cues
    << ",\"glyphs\":" << out.stats.glyphs
    << ",\"breaks\":" << out.stats.breaks
    << ",\"overflow_count\":" << out.stats.overflow_count
    << ",\"missing_glyph\":" << out.stats.missing_glyph << "},\n";
  o << "  \"cues\":[\n";
  for (size_t i = 0; i < out.cues.size(); ++i) {
    o << ind(4);
    std::string cj = cue_metrics_json(out.cues[i], 4);
    o << cj << (i + 1 < out.cues.size() ? "," : "") << "\n";
  }
  o << "  ],\n";
  o << "  \"overflow_list\":[";
  bool first = true;
  for (const auto& m : out.cues) {
    if (!m.overflow) continue;
    if (!first) o << ",";
    first = false;
    o << "\n    {\"cue_id\":\"" << esc(m.cue_id)
      << "\",\"overflow_px\":" << m.overflow_px
      << ",\"rules\":" << rules_json(m.rules) << "}";
  }
  if (!first) o << "\n  ";
  o << "]\n}\n";
  return o.str();
}

std::string jsonl_from_records(const std::vector<CueRecord>& recs) {
  std::ostringstream o;
  for (const auto& m : recs) {
    o << "{\"cue_id\":\"" << esc(m.cue_id) << "\",\"line_count\":" << m.line_count
      << ",\"breaks\":" << breaks_json(m.breaks)
      << ",\"overflow\":" << (m.overflow ? "true" : "false")
      << ",\"overflow_px\":" << m.overflow_px << "}\n";
  }
  return o.str();
}

bool write_file(const std::string& path, const std::string& data, std::string& err) {
  std::ofstream f(path, std::ios::binary | std::ios::trunc);
  if (!f) {
    err = "cannot open for write: " + path;
    return false;
  }
  f << data;
  f.flush();
  if (!f) {
    err = "write failed: " + path;
    return false;
  }
  return true;
}

static bool scan_long(const std::string& json, size_t colon, size_t limit,
                      long& out) {
  size_t p = colon + 1;
  while (p < limit && (json[p] == ' ' || json[p] == '\n')) ++p;
  bool neg = p < limit && json[p] == '-';
  if (neg) ++p;
  if (p >= limit || !isdigit(static_cast<unsigned char>(json[p]))) return false;
  long v = 0;
  while (p < limit && isdigit(static_cast<unsigned char>(json[p]))) {
    v = v * 10 + (json[p] - '0');
    ++p;
  }
  out = neg ? -v : v;
  return true;
}

// Minimal JSON scanning: extract cue records in document order.
bool json_read_cues(const std::string& json, std::vector<CueRecord>& out) {
  out.clear();
  size_t pos = 0;
  auto find_key = [&](const std::string& key, size_t from, size_t limit) {
    std::string pat = "\"" + key + "\"";
    size_t p = json.find(pat, from);
    if (p == std::string::npos || p >= limit) return std::string::npos;
    return p + pat.size();
  };
  while (true) {
    size_t id = find_key("cue_id", pos, json.size());
    if (id == std::string::npos) break;
    size_t colon = json.find(':', id);
    size_t q1 = json.find('"', colon + 1);
    size_t q2 = json.find('"', q1 + 1);
    if (colon == std::string::npos || q1 == std::string::npos ||
        q2 == std::string::npos)
      return false;
    CueRecord rec;
    rec.cue_id = json.substr(q1 + 1, q2 - q1 - 1);
    // breaks array must appear before the next cue_id (or end)
    size_t next_id = json.find("\"cue_id\"", q2);
    size_t limit = next_id == std::string::npos ? json.size() : next_id;
    size_t bk = find_key("breaks", q2, limit);
    if (bk == std::string::npos) {
      pos = q2;  // record without breaks (e.g. overflow_list entry): skip
      continue;
    }
    size_t ob = json.find('[', bk);
    size_t cb = json.find(']', ob);
    if (ob == std::string::npos || cb == std::string::npos || cb > limit)
      return false;
    std::vector<size_t> vals;
    size_t p = ob + 1;
    while (p < cb) {
      while (p < cb && (json[p] == ' ' || json[p] == ',' || json[p] == '\n'))
        ++p;
      if (p >= cb) break;
      if (!isdigit(static_cast<unsigned char>(json[p]))) return false;
      size_t v = 0;
      while (p < cb && isdigit(static_cast<unsigned char>(json[p]))) {
        v = v * 10 + static_cast<size_t>(json[p] - '0');
        ++p;
      }
      vals.push_back(v);
    }
    rec.breaks = vals;
    size_t lc = find_key("line_count", q2, limit);
    if (lc != std::string::npos) {
      long v = 0;
      size_t c2 = json.find(':', lc);
      if (c2 != std::string::npos && scan_long(json, c2, limit, v))
        rec.line_count = v;
    }
    size_t op = find_key("overflow_px", q2, limit);
    if (op != std::string::npos) {
      long v = 0;
      size_t c2 = json.find(':', op);
      if (c2 != std::string::npos && scan_long(json, c2, limit, v))
        rec.overflow_px = v;
    }
    size_t ov = find_key("overflow", q2, limit);
    if (ov != std::string::npos) {
      size_t c2 = json.find(':', ov);
      rec.overflow = c2 != std::string::npos &&
                     json.compare(c2 + 1, 4, "true") == 0;
    }
    out.push_back(rec);
    pos = cb + 1;
  }
  return !out.empty();
}

bool json_get_int(const std::string& json, const std::string& key, long& out) {
  std::string pat = "\"" + key + "\"";
  size_t p = json.find(pat);
  if (p == std::string::npos) return false;
  p = json.find(':', p + pat.size());
  if (p == std::string::npos) return false;
  ++p;
  while (p < json.size() && (json[p] == ' ' || json[p] == '\n')) ++p;
  bool neg = p < json.size() && json[p] == '-';
  if (neg) ++p;
  if (p >= json.size() || !isdigit(static_cast<unsigned char>(json[p])))
    return false;
  long v = 0;
  while (p < json.size() && isdigit(static_cast<unsigned char>(json[p]))) {
    v = v * 10 + (json[p] - '0');
    ++p;
  }
  out = neg ? -v : v;
  return true;
}

}  // namespace sublay
