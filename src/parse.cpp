#include "types.h"

#include <fstream>
#include <set>
#include <sstream>

namespace sublay {

static bool parse_ts(const std::string& s, int64_t& ms) {
  // HH:MM:SS,mmm
  if (s.size() != 12 || s[2] != ':' || s[5] != ':' || s[8] != ',') return false;
  auto dig = [&](size_t a, size_t b, long& v) {
    v = 0;
    for (size_t i = a; i < b; ++i) {
      if (s[i] < '0' || s[i] > '9') return false;
      v = v * 10 + (s[i] - '0');
    }
    return true;
  };
  long h, m, sec, milli;
  if (!dig(0, 2, h) || !dig(3, 5, m) || !dig(6, 8, sec) || !dig(9, 12, milli))
    return false;
  if (m > 59 || sec > 59) return false;
  ms = ((h * 60 + m) * 60 + sec) * 1000 + milli;
  return true;
}

bool strip_markup(const std::string& raw, StyledText& out, std::string& err) {
  out = StyledText{};
  std::vector<std::pair<char, size_t>> open;  // tag, plain-text start
  auto close = [&](char tag) {
    for (size_t i = open.size(); i-- > 0;) {
      if (open[i].first == tag) {
        size_t start = open[i].second;
        open.erase(open.begin() + static_cast<long>(i));
        auto& dst = tag == 'b' ? out.bold : tag == 'i' ? out.italic : out.underline;
        dst.emplace_back(start, out.text.size());
        return true;
      }
    }
    return false;
  };
  for (size_t i = 0; i < raw.size();) {
    char c = raw[i];
    if (c == '\\') {
      if (i + 1 < raw.size() && raw[i + 1] == 'n') {
        out.forced_breaks.push_back(out.text.size());
        i += 2;
        continue;
      }
      if (i + 1 < raw.size()) {
        out.text.push_back(raw[i + 1]);
        i += 2;
        continue;
      }
      err = "dangling escape";
      return false;
    }
    if (c == '<') {
      size_t gt = raw.find('>', i);
      if (gt == std::string::npos) { err = "unterminated tag"; return false; }
      std::string t = raw.substr(i + 1, gt - i - 1);
      if (t == "b" || t == "i" || t == "u") {
        open.emplace_back(t[0], out.text.size());
      } else if (t == "/b" || t == "/i" || t == "/u") {
        if (!close(t[1])) { err = "unmatched closing tag </" + t + ">"; return false; }
      } else {
        err = "unknown tag <" + t + ">";
        return false;
      }
      i = gt + 1;
      continue;
    }
    out.text.push_back(c);
    ++i;
  }
  if (!open.empty()) { err = "unclosed inline tag"; return false; }
  return true;
}

ParseResult parse_subtxt(const std::string& path) {
  ParseResult res;
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    res.issues.push_back({0, 0, "cannot open file"});
    return res;
  }
  std::stringstream ss;
  ss << in.rdbuf();
  std::string data = ss.str();

  std::vector<std::pair<std::string, size_t>> lines;  // text, 1-based line no
  {
    size_t pos = 0;
    size_t no = 1;
    while (pos <= data.size()) {
      size_t nl = data.find('\n', pos);
      if (nl == std::string::npos) {
        if (pos < data.size()) lines.emplace_back(data.substr(pos), no);
        break;
      }
      std::string ln = data.substr(pos, nl - pos);
      if (!ln.empty() && ln.back() == '\r') ln.pop_back();
      lines.emplace_back(ln, no);
      pos = nl + 1;
      ++no;
    }
  }

  std::set<std::string> seen_ids;
  size_t i = 0;
  while (i < lines.size()) {
    const std::string& ln = lines[i].first;
    if (ln.empty() || ln[0] == '#') { ++i; continue; }
    if (ln.rfind("=== cue", 0) != 0) {
      res.issues.push_back({static_cast<int>(lines[i].second), 1,
                            "expected '=== cue' block header"});
      ++i;
      continue;
    }
    size_t head_line = lines[i].second;
    ++i;
    Cue cue;
    bool have_id = false, have_lang = false, have_time = false, have_text = false;
    std::string raw_text;
    std::string err;
    size_t err_line = head_line, err_col = 1;
    while (i < lines.size() && lines[i].first.rfind("=== cue", 0) != 0) {
      const std::string& l = lines[i].first;
      size_t lno = lines[i].second;
      ++i;
      if (l.empty() || l[0] == '#') continue;
      size_t colon = l.find(':');
      if (colon == std::string::npos || colon == 0) {
        err = "malformed field line";
        err_line = lno;
        break;
      }
      std::string key = l.substr(0, colon);
      std::string val = l.substr(colon + 1);
      while (!val.empty() && val.front() == ' ') val.erase(val.begin());
      if (key == "id") {
        cue.id = val;
        have_id = !val.empty();
      } else if (key == "lang") {
        cue.lang = val;
        have_lang = !val.empty();
      } else if (key == "speaker") {
        cue.speaker = val;
      } else if (key == "time") {
        size_t sep = val.find("-->");
        int64_t a = 0, b = 0;
        std::string left = sep == std::string::npos ? val : val.substr(0, sep);
        std::string right = sep == std::string::npos ? "" : val.substr(sep + 3);
        while (!left.empty() && left.back() == ' ') left.pop_back();
        while (!right.empty() && right.front() == ' ') right.erase(right.begin());
        if (sep == std::string::npos || !parse_ts(left, a) || !parse_ts(right, b)) {
          err = "bad timeline";
          err_line = lno;
          break;
        }
        if (b < a) { err = "timeline reversed"; err_line = lno; break; }
        cue.start_ms = a;
        cue.end_ms = b;
        have_time = true;
      } else if (key == "text") {
        raw_text = val;
        have_text = true;
      }
    }
    if (err.empty()) {
      if (!have_id || !have_lang || !have_time || !have_text)
        err = "missing required field (id/lang/time/text)";
      else if (seen_ids.count(cue.id))
        err = "duplicate cue_id '" + cue.id + "'";
    }
    if (err.empty()) {
      std::vector<UChar> tmp;
      size_t bad = 0;
      if (!utf8_decode(raw_text, tmp, bad)) {
        err = "invalid utf-8 byte";
        err_col = bad + 1;
      }
    }
    StyledText st;
    if (err.empty() && !strip_markup(raw_text, st, err)) err_line = head_line;
    if (!err.empty()) {
      res.issues.push_back({static_cast<int>(err_line), static_cast<int>(err_col), err});
      while (i < lines.size() && lines[i].first.rfind("=== cue", 0) != 0) ++i;
      continue;  // skip bad cue, keep the batch going
    }
    seen_ids.insert(cue.id);
    cue.text = raw_text;
    res.cues.push_back(cue);
  }
  return res;
}

}  // namespace sublay
