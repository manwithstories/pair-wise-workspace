#include "cue_parse.hpp"

#include <algorithm>
#include <cctype>
#include <fstream>
#include <sstream>
#include <iomanip>

#include "utf8.hpp"

namespace smx {
namespace {
struct RawLine {
  std::size_t number = 0;
  std::string text;
};

std::string trim(std::string s) {
  const auto first = s.find_first_not_of(" \t");
  if (first == std::string::npos) return {};
  const auto last = s.find_last_not_of(" \t");
  return s.substr(first, last - first + 1);
}

std::vector<std::string> tokens(std::string text, bool& quoted_error) {
  std::vector<std::string> out;
  std::string current;
  bool quoted = false;
  char quote = 0;
  for (char c : text) {
    if (quoted) {
      if (c == quote) quoted = false;
      else current.push_back(c);
    } else if (c == '"' || c == '\'') {
      quoted = true;
      quote = c;
    } else if (std::isspace(static_cast<unsigned char>(c))) {
      if (!current.empty()) { out.push_back(current); current.clear(); }
    } else {
      current.push_back(c);
    }
  }
  if (quoted) quoted_error = true;
  if (!current.empty()) out.push_back(current);
  return out;
}

bool parse_attributes(const std::vector<std::string>& words, std::map<std::string, std::string>& out,
                      std::string& first_bare) {
  for (const auto& word : words) {
    const auto eq = word.find('=');
    if (eq == std::string::npos) {
      if (first_bare.empty()) first_bare = word;
      continue;
    }
    if (eq == 0 || eq + 1 >= word.size()) return false;
    out[word.substr(0, eq)] = word.substr(eq + 1);
  }
  return true;
}

std::string unescape_text(const std::string& text) {
  std::string out;
  auto hex = [](char c) -> int {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
  };
  for (std::size_t i = 0; i < text.size(); ++i) {
    if (text[i] == '\\' && i + 1 < text.size()) {
      const char c = text[i + 1];
      if (c == 'n') { out.push_back('\n'); ++i; continue; }
      if (c == 't') { out.push_back('\t'); ++i; continue; }
      if (c == '\\') { out.push_back('\\'); ++i; continue; }
      if (c == 'u' && i + 5 < text.size()) {
        int value = 0;
        bool ok = true;
        for (int k = 2; k <= 5; ++k) {
          const int h = hex(text[i + k]);
          if (h < 0) { ok = false; break; }
          value = value * 16 + h;
        }
        if (ok && !(value >= 0xd800 && value <= 0xdfff)) {
          out += utf8_encode(static_cast<std::uint32_t>(value));
          i += 5;
          continue;
        }
      }
      if (c == 'x' && i + 3 < text.size()) {
        const int hi = hex(text[i + 2]);
        const int lo = hex(text[i + 3]);
        if (hi >= 0 && lo >= 0) {
          out.push_back(static_cast<char>(hi * 16 + lo));
          i += 3;
          continue;
        }
      }
    }
    out.push_back(text[i]);
  }
  return out;
}
std::string make_auto_id(std::size_t index) {
  std::ostringstream ss;
  ss << "cue_" << std::setw(4) << std::setfill('0') << index;
  return ss.str();
}
}  // namespace

CueDocument parse_cues(std::string_view source) {
  CueDocument doc;
  std::vector<RawLine> lines;
  std::size_t line_no = 0;
  std::size_t start = 0;
  while (start <= source.size()) {
    const auto nl = source.find('\n', start);
    const auto end = nl == std::string_view::npos ? source.size() : nl;
    std::string text(source.substr(start, end - start));
    if (!text.empty() && text.back() == '\r') text.pop_back();
    lines.push_back({++line_no, text});
    if (nl == std::string_view::npos) break;
    start = nl + 1;
  }
  if (!lines.empty() && lines.front().text.rfind("\xef\xbb\xbf", 0) == 0)
    lines.front().text.erase(0, 3);

  bool active = false;
  Cue current;
  std::string segment;
  std::string auto_style = "default";
  std::size_t implicit_index = 0;

  auto finish = [&](std::size_t at_line) {
    if (!active) return;
    active = false;
    if (current.text.empty()) {
      doc.errors.push_back({at_line, "empty cue '" + current.id + "'"});
      return;
    }
    const bool duplicate = std::any_of(doc.cues.begin(), doc.cues.end(),
                                       [&](const Cue& c) { return c.id == current.id; });
    if (duplicate) {
      doc.errors.push_back({current.line, "duplicate cue id '" + current.id + "'"});
      return;
    }
    doc.cues.push_back(current);
  };

  auto begin_cue = [&](std::size_t at_line, const std::string& id,
                       const std::map<std::string, std::string>& attrs) {
    finish(at_line);
    current = Cue{};
    current.line = at_line;
    current.segment = segment;
    current.style = auto_style;
    auto id_it = attrs.find("id");
    current.id = id_it != attrs.end() ? id_it->second : id;
    if (current.id.empty()) current.id = make_auto_id(++implicit_index);
    else ++implicit_index;
    auto style_it = attrs.find("style");
    if (style_it != attrs.end()) current.style = style_it->second;
    auto seg_it = attrs.find("segment");
    if (seg_it != attrs.end()) current.segment = seg_it->second;
    active = true;
  };

  for (std::size_t i = 0; i < lines.size(); ++i) {
    const auto& raw = lines[i];
    std::string text = raw.text;
    std::string stripped = trim(text);
    if (stripped.empty()) {
      finish(raw.number);
      continue;
    }
    if (stripped.rfind("//", 0) == 0 || (stripped[0] == '#' && stripped.rfind("##", 0) != 0)) {
      continue;
    }
    if (stripped.rfind("##", 0) == 0) {
      bool quote_error = false;
      auto words = tokens(stripped.substr(2), quote_error);
      if (quote_error || words.empty()) {
        doc.errors.push_back({raw.number, "malformed cue marker"});
        continue;
      }
      std::map<std::string, std::string> attrs;
      std::string bare;
      if (!parse_attributes(words, attrs, bare)) {
        doc.errors.push_back({raw.number, "malformed cue attributes"});
        continue;
      }
      begin_cue(raw.number, bare, attrs);
      continue;
    }
    if (stripped.front() == '[') {
      if (stripped.back() != ']') {
        doc.errors.push_back({raw.number, "unterminated directive"});
        continue;
      }
      bool quote_error = false;
      auto words = tokens(stripped.substr(1, stripped.size() - 2), quote_error);
      if (quote_error || words.empty()) {
        doc.errors.push_back({raw.number, "malformed directive"});
        continue;
      }
      const std::string kind = words.front();
      if (kind == "cue") {
        std::map<std::string, std::string> attrs;
        std::string bare;
        if (!parse_attributes({words.begin() + 1, words.end()}, attrs, bare)) {
          doc.errors.push_back({raw.number, "malformed cue attributes"});
          continue;
        }
        begin_cue(raw.number, bare, attrs);
      } else if (kind == "style") {
        std::map<std::string, std::string> attrs;
        std::string bare;
        if (!parse_attributes({words.begin() + 1, words.end()}, attrs, bare) || bare.empty()) {
          doc.errors.push_back({raw.number, "style header needs a name"});
          continue;
        }
        CueStyle style;
        style.name = bare;
        style.attributes = attrs;
        style.line = raw.number;
        doc.styles.push_back(style);
      } else if (kind == "segment") {

        if (words.size() > 1)
          segment = words[1].find("name=") == 0 ? words[1].substr(5) : words[1];
      } else if (kind == "styles" || kind == "cues") {
        continue;
      } else {
        doc.errors.push_back({raw.number, "unknown directive '" + kind + "'"});
      }
      continue;
    }
    if (stripped.rfind("STYLE", 0) == 0 &&
        (stripped.size() == 5 || std::isspace(static_cast<unsigned char>(stripped[5])))) {
      bool quote_error = false;
      auto words = tokens(stripped.substr(5), quote_error);
      if (quote_error || words.empty()) {
        doc.errors.push_back({raw.number, "malformed STYLE header"});
        continue;
      }
      CueStyle style;
      style.name = words.front();
      std::map<std::string, std::string> attrs;
      std::string bare;
      if (!parse_attributes({words.begin() + 1, words.end()}, attrs, bare)) {
        doc.errors.push_back({raw.number, "malformed STYLE attributes"});
        continue;
      }
      style.attributes = attrs;
      style.line = raw.number;
      doc.styles.push_back(style);
      auto_style = style.name;
      continue;
    }
    if (stripped.find("-->") != std::string::npos) {
      continue;
    }
    if (active) {
      const std::string decoded = unescape_text(text);
      const auto utf = decode_utf8(decoded);
      doc.invalid_utf8 += utf.invalid_sequences;
      current.invalid_utf8 += utf.invalid_sequences;
      if (!current.text.empty()) current.text.push_back('\n');
      current.text += utf8_encode(utf.text);
      continue;
    }
    const std::string decoded = unescape_text(text);
    const auto utf = decode_utf8(decoded);
    doc.invalid_utf8 += utf.invalid_sequences;
    Cue cue;
    cue.id = make_auto_id(++implicit_index);
    cue.text = utf8_encode(utf.text);
    cue.segment = segment;
    cue.style = auto_style;
    cue.line = raw.number;
    cue.invalid_utf8 = utf.invalid_sequences;
    doc.cues.push_back(std::move(cue));
  }
  finish(lines.empty() ? 0 : lines.back().number);
  return doc;
}

CueDocument parse_cue_file(const std::string& path, std::string& io_error) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    io_error = "cannot open cue file: " + path;
    return {};
  }
  std::ostringstream ss;
  ss << in.rdbuf();
  if (!in.good() && !in.eof()) {
    io_error = "cannot read cue file: " + path;
    return {};
  }
  return parse_cues(ss.str());
}
}  // namespace smx
