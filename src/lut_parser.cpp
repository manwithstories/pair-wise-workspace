#include "lut_parser.hpp"

#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <fstream>
#include <sstream>

namespace lutgrade {
namespace {

[[noreturn]] void fail(const std::string& origin, std::size_t line,
                       const std::string& what) {
  std::ostringstream msg;
  msg << origin << ':' << line << ": " << what;
  throw Error(ExitCode::kUsage, "cube_invalid", msg.str());
}

bool is_blank(char c) { return c == ' ' || c == '\t' || c == '\r'; }

std::string trim(const std::string& s) {
  std::size_t b = 0;
  std::size_t e = s.size();
  while (b < e && (is_blank(s[b]) || s[b] == '\n' || s[b] == '\v')) ++b;
  while (e > b && (is_blank(s[e - 1]) || s[e - 1] == '\n' || s[e - 1] == '\v')) --e;
  return s.substr(b, e - b);
}

std::vector<std::string> split_tokens(const std::string& s) {
  std::vector<std::string> out;
  std::size_t i = 0;
  while (i < s.size()) {
    while (i < s.size() && (is_blank(s[i]))) ++i;
    const std::size_t start = i;
    while (i < s.size() && !is_blank(s[i])) ++i;
    if (i > start) out.push_back(s.substr(start, i - start));
  }
  return out;
}

// Strict decimal parse: the whole token must be consumed and the result
// must be finite. Rejects "nan", "inf", trailing junk, and overflow.
bool parse_number(const std::string& token, double& out) {
  if (token.empty()) return false;
  errno = 0;
  char* end = nullptr;
  const double value = std::strtod(token.c_str(), &end);
  if (end != token.c_str() + token.size()) return false;
  if (errno == ERANGE) return false;
  if (!std::isfinite(value)) return false;
  out = value;
  return true;
}

bool is_supported_size(int n) {
  for (const int candidate : kSupportedGridSizes) {
    if (n == candidate) return true;
  }
  return false;
}

// Parse a directive's three numeric operands; the caller has already checked
// the token count.
std::array<double, 3> parse_triplet(const std::string& origin, std::size_t line,
                                    const std::vector<std::string>& tokens) {
  std::array<double, 3> out{};
  for (std::size_t i = 0; i < 3; ++i) {
    if (!parse_number(tokens[i + 1], out[i])) {
      fail(origin, line, "not a finite number: '" + tokens[i + 1] + "'");
    }
  }
  return out;
}

}  // namespace

Lut3D parse_cube(const std::string& text, const std::string& origin) {
  Lut3D lut;
  bool have_size = false;
  bool have_domain_min = false;
  bool have_domain_max = false;
  bool have_title = false;
  bool saw_data = false;
  std::size_t data_lines = 0;

  // Skip a UTF-8 BOM if the file was saved with one.
  std::size_t cursor = 0;
  if (text.size() >= 3 && static_cast<unsigned char>(text[0]) == 0xEF &&
      static_cast<unsigned char>(text[1]) == 0xBB &&
      static_cast<unsigned char>(text[2]) == 0xBF) {
    cursor = 3;
  }

  std::size_t line_no = 0;
  while (cursor <= text.size()) {
    const std::size_t nl = text.find('\n', cursor);
    const std::size_t end = (nl == std::string::npos) ? text.size() : nl;
    std::string line = text.substr(cursor, end - cursor);
    cursor = (nl == std::string::npos) ? text.size() + 1 : nl + 1;
    ++line_no;

    const std::size_t hash = line.find('#');
    if (hash != std::string::npos) line.erase(hash);
    line = trim(line);
    if (line.empty()) continue;

    const std::vector<std::string> tokens = split_tokens(line);
    const std::string& head = tokens[0];

    if (head == "TITLE") {
      if (saw_data) fail(origin, line_no, "TITLE after LUT data");
      if (have_title) fail(origin, line_no, "duplicate TITLE");
      if (tokens.size() != 2 || tokens[1].size() < 2 ||
          tokens[1].front() != '"' || tokens[1].back() != '"') {
        fail(origin, line_no, "TITLE expects one double-quoted string");
      }
      if (tokens[1].find('"', 1) != tokens[1].size() - 1) {
        fail(origin, line_no, "TITLE contains an embedded quote");
      }
      lut.title = tokens[1].substr(1, tokens[1].size() - 2);
      have_title = true;
      continue;
    }

    if (head == "LUT_3D_SIZE") {
      if (saw_data) fail(origin, line_no, "LUT_3D_SIZE after LUT data");
      if (have_size) fail(origin, line_no, "duplicate LUT_3D_SIZE");
      if (tokens.size() != 2) fail(origin, line_no, "LUT_3D_SIZE expects one integer");
      double raw = 0.0;
      if (!parse_number(tokens[1], raw) || raw != std::floor(raw)) {
        fail(origin, line_no, "LUT_3D_SIZE is not an integer: '" + tokens[1] + "'");
      }
      if (raw <= 1.0 || raw > 1024.0) {
        fail(origin, line_no, "LUT_3D_SIZE out of range: " + tokens[1]);
      }
      const int n = static_cast<int>(raw);
      if (!is_supported_size(n)) {
        fail(origin, line_no,
             "unsupported grid size " + tokens[1] + " (expected 17, 33 or 65)");
      }
      lut.size = n;
      have_size = true;
      continue;
    }

    if (head == "LUT_1D_SIZE" || head == "LUT_1D_INPUT_RANGE") {
      fail(origin, line_no, "1D LUT directive '" + head + "' is not a cube");
    }

    if (head == "DOMAIN_MIN" || head == "DOMAIN_MAX" ||
        head == "LUT_3D_INPUT_RANGE") {
      if (saw_data) fail(origin, line_no, head + " after LUT data");
      // DOMAIN_MIN/MAX are independent declarations; the combined range form
      // owns both, so mixing them is a genuine duplicate.
      if (head == "LUT_3D_INPUT_RANGE") {
        if (have_domain_min || have_domain_max) {
          fail(origin, line_no, "duplicate domain declaration");
        }
        // Two scalars apply to every channel.
        if (tokens.size() != 3) {
          fail(origin, line_no, "LUT_3D_INPUT_RANGE expects 'lo hi'");
        }
        double lo = 0.0;
        double hi = 0.0;
        if (!parse_number(tokens[1], lo)) {
          fail(origin, line_no, "not a finite number: '" + tokens[1] + "'");
        }
        if (!parse_number(tokens[2], hi)) {
          fail(origin, line_no, "not a finite number: '" + tokens[2] + "'");
        }
        lut.domain_min = {lo, lo, lo};
        lut.domain_max = {hi, hi, hi};
        have_domain_min = have_domain_max = true;
      } else {
        bool& seen = (head == "DOMAIN_MIN") ? have_domain_min : have_domain_max;
        if (seen) fail(origin, line_no, "duplicate " + head);
        if (tokens.size() != 4) fail(origin, line_no, head + " expects 'r g b'");
        auto& target = (head == "DOMAIN_MIN") ? lut.domain_min : lut.domain_max;
        target = parse_triplet(origin, line_no, tokens);
        seen = true;
      }
      continue;
    }

    // Anything else must be a data triplet; a non-numeric leading token is a
    // directive we do not understand. Numeric-looking garbage (nan, inf,
    // 1e999) falls through to the value parser so it is reported as a bad
    // number rather than an unknown directive.
    const char lead = head[0];
    const bool numeric_lead = (lead >= '0' && lead <= '9') || lead == '-' ||
                              lead == '+' || lead == '.';
    const bool special_float = head == "nan" || head == "inf" ||
                               head == "-inf" || head == "+inf" ||
                               head == "infinity";
    if (!numeric_lead && !special_float) {
      fail(origin, line_no, "unrecognised directive '" + head + "'");
    }
    if (tokens.size() != 3) {
      fail(origin, line_no, "LUT data line must have exactly 3 values");
    }
    if (!have_size) fail(origin, line_no, "LUT data before LUT_3D_SIZE");
    if (data_lines >= lut.entry_count()) {
      fail(origin, line_no, "extra LUT data beyond " +
                                std::to_string(lut.entry_count()) + " entries");
    }
    std::array<float, 3> entry{};
    for (std::size_t i = 0; i < 3; ++i) {
      double value = 0.0;
      if (!parse_number(tokens[i], value)) {
        fail(origin, line_no, "invalid LUT value '" + tokens[i] + "'");
      }
      if (value < 0.0 || value > 1.0) {
        fail(origin, line_no, "LUT value out of [0,1]: " + tokens[i]);
      }
      entry[i] = static_cast<float>(value);
    }
    lut.table.push_back(entry);
    saw_data = true;
    ++data_lines;
  }

  if (!have_size) fail(origin, line_no, "missing LUT_3D_SIZE");
  if (!saw_data) fail(origin, line_no, "missing LUT data");
  if (data_lines != lut.entry_count()) {
    fail(origin, line_no, "LUT data has " + std::to_string(data_lines) +
                              " entries, expected " +
                              std::to_string(lut.entry_count()));
  }
  for (std::size_t c = 0; c < 3; ++c) {
    if (!(lut.domain_max[c] > lut.domain_min[c])) {
      fail(origin, line_no, "DOMAIN_MAX must be greater than DOMAIN_MIN");
    }
  }
  return lut;
}

Lut3D load_cube(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    throw Error(ExitCode::kUsage, "cube_io", "cannot open LUT: " + path);
  }
  std::ostringstream buf;
  buf << in.rdbuf();
  if (in.bad()) {
    throw Error(ExitCode::kUsage, "cube_io", "failed reading LUT: " + path);
  }
  return parse_cube(buf.str(), path);
}

}  // namespace lutgrade
