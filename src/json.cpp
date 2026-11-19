#include "json.hpp"

#include <cerrno>
#include <charconv>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <limits>
#include <sstream>

namespace media_restore {
namespace {

constexpr int kMaxDepth = 64;
constexpr size_t kMaxNodes = 1000000;

struct Parser {
  std::string_view text;
  size_t pos = 0;
  size_t nodes = 0;

  bool eof() const { return pos >= text.size(); }
  char peek() const { return eof() ? '\0' : text[pos]; }

  bool fail(size_t at, const char* message) {
    error_ = std::string(message) + " at byte " + std::to_string(at);
    error_pos_ = at;
    return false;
  }

  void skip_ws() {
    while (!eof()) {
      const char c = text[pos];
      if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
        ++pos;
      } else {
        break;
      }
    }
  }

  bool consume(char expected) {
    if (peek() != expected) {
      return fail(pos, "unexpected character");
    }
    ++pos;
    return true;
  }

  bool parse_hex4(uint32_t* out) {
    if (pos + 4 > text.size()) return fail(pos, "truncated unicode escape");
    uint32_t value = 0;
    for (int i = 0; i < 4; ++i) {
      const char c = text[pos++];
      value <<= 4;
      if (c >= '0' && c <= '9') {
        value |= static_cast<uint32_t>(c - '0');
      } else if (c >= 'a' && c <= 'f') {
        value |= static_cast<uint32_t>(c - 'a' + 10);
      } else if (c >= 'A' && c <= 'F') {
        value |= static_cast<uint32_t>(c - 'A' + 10);
      } else {
        return fail(pos - 1, "invalid unicode escape");
      }
    }
    *out = value;
    return true;
  }

  static void append_utf8(std::string* out, uint32_t code) {
    if (code < 0x80) {
      out->push_back(static_cast<char>(code));
    } else if (code < 0x800) {
      out->push_back(static_cast<char>(0xC0 | (code >> 6)));
      out->push_back(static_cast<char>(0x80 | (code & 0x3F)));
    } else if (code < 0x10000) {
      out->push_back(static_cast<char>(0xE0 | (code >> 12)));
      out->push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
      out->push_back(static_cast<char>(0x80 | (code & 0x3F)));
    } else {
      out->push_back(static_cast<char>(0xF0 | (code >> 18)));
      out->push_back(static_cast<char>(0x80 | ((code >> 12) & 0x3F)));
      out->push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
      out->push_back(static_cast<char>(0x80 | (code & 0x3F)));
    }
  }

  bool parse_string(std::string* out) {
    if (!consume('"')) return false;
    while (!eof()) {
      const unsigned char c = static_cast<unsigned char>(text[pos]);
      if (c == '"') {
        ++pos;
        return true;
      }
      if (c < 0x20) return fail(pos, "control character in string");
      if (c != '\\') {
        out->push_back(static_cast<char>(c));
        ++pos;
        continue;
      }
      ++pos;
      if (eof()) return fail(pos, "truncated escape");
      const char esc = text[pos++];
      switch (esc) {
        case '"': out->push_back('"'); break;
        case '\\': out->push_back('\\'); break;
        case '/': out->push_back('/'); break;
        case 'b': out->push_back('\b'); break;
        case 'f': out->push_back('\f'); break;
        case 'n': out->push_back('\n'); break;
        case 'r': out->push_back('\r'); break;
        case 't': out->push_back('\t'); break;
        case 'u': {
          uint32_t code = 0;
          if (!parse_hex4(&code)) return false;
          if (code >= 0xD800 && code <= 0xDBFF) {
            if (pos + 2 > text.size() || text[pos] != '\\' || text[pos + 1] != 'u') {
              return fail(pos, "missing low surrogate");
            }
            pos += 2;
            uint32_t low = 0;
            if (!parse_hex4(&low)) return false;
            if (low < 0xDC00 || low > 0xDFFF) return fail(pos, "invalid low surrogate");
            code = 0x10000 + ((code - 0xD800) << 10) + (low - 0xDC00);
          }
          append_utf8(out, code);
          break;
        }
        default:
          return fail(pos - 1, "unknown escape");
      }
    }
    return fail(text.size(), "unterminated string");
  }

  bool parse_number(Json* out) {
    const size_t start = pos;
    if (peek() == '-') ++pos;
    if (eof()) return fail(start, "truncated number");
    if (peek() == '0') {
      ++pos;
    } else if (peek() >= '1' && peek() <= '9') {
      while (!eof() && peek() >= '0' && peek() <= '9') ++pos;
    } else {
      return fail(start, "invalid number");
    }
    bool is_float = false;
    if (peek() == '.') {
      is_float = true;
      ++pos;
      if (eof() || peek() < '0' || peek() > '9') return fail(pos, "invalid fraction");
      while (!eof() && peek() >= '0' && peek() <= '9') ++pos;
    }
    if (peek() == 'e' || peek() == 'E') {
      is_float = true;
      ++pos;
      if (peek() == '+' || peek() == '-') ++pos;
      if (eof() || peek() < '0' || peek() > '9') return fail(pos, "invalid exponent");
      while (!eof() && peek() >= '0' && peek() <= '9') ++pos;
    }
    const std::string token(text.substr(start, pos - start));
    if (!is_float) {
      errno = 0;
      char* end = nullptr;
      const long long value = std::strtoll(token.c_str(), &end, 10);
      if (errno == ERANGE || end != token.c_str() + token.size()) {
        return fail(start, "integer out of range");
      }
      *out = Json(static_cast<int64_t>(value));
      return true;
    }
    errno = 0;
    char* end = nullptr;
    const double value = std::strtod(token.c_str(), &end);
    if (errno == ERANGE || end != token.c_str() + token.size() ||
        !std::isfinite(value)) {
      return fail(start, "number out of range");
    }
    *out = Json(value);
    return true;
  }

  bool parse_value(Json* out, int depth) {
    if (++nodes > kMaxNodes) return fail(pos, "document exceeds node limit");
    if (depth > kMaxDepth) return fail(pos, "document exceeds nesting limit");
    skip_ws();
    if (eof()) return fail(pos, "unexpected end of input");
    const char c = peek();
    if (c == 'n') {
      if (text.substr(pos, 4) != "null") return fail(pos, "invalid literal");
      pos += 4;
      *out = Json();
      return true;
    }
    if (c == 't' || c == 'f') {
      const bool is_true = c == 't';
      const std::string_view lit = is_true ? "true" : "false";
      if (text.substr(pos, lit.size()) != lit) return fail(pos, "invalid literal");
      pos += lit.size();
      *out = Json(is_true);
      return true;
    }
    if (c == '"') {
      std::string value;
      if (!parse_string(&value)) return false;
      *out = Json(std::move(value));
      return true;
    }
    if (c == '-' || (c >= '0' && c <= '9')) return parse_number(out);
    if (c == '[') {
      ++pos;
      *out = Json::array();
      skip_ws();
      if (peek() == ']') {
        ++pos;
        return true;
      }
      while (true) {
        Json item;
        if (!parse_value(&item, depth + 1)) return false;
        out->push(std::move(item));
        skip_ws();
        if (peek() == ',') {
          ++pos;
          continue;
        }
        if (peek() == ']') {
          ++pos;
          return true;
        }
        return fail(pos, "expected ',' or ']'");
      }
    }
    if (c == '{') {
      ++pos;
      *out = Json::object();
      skip_ws();
      if (peek() == '}') {
        ++pos;
        return true;
      }
      while (true) {
        skip_ws();
        if (peek() != '"') return fail(pos, "expected object key");
        std::string key;
        if (!parse_string(&key)) return false;
        if (out->find(key) != nullptr) return fail(pos, "duplicate object key");
        skip_ws();
        if (!consume(':')) return false;
        Json value;
        if (!parse_value(&value, depth + 1)) return false;
        out->set(std::move(key), std::move(value));
        skip_ws();
        if (peek() == ',') {
          ++pos;
          continue;
        }
        if (peek() == '}') {
          ++pos;
          return true;
        }
        return fail(pos, "expected ',' or '}'");
      }
    }
    return fail(pos, "unexpected character");
  }

  std::string error_;
  size_t error_pos_ = 0;
};

void dump_string(const std::string& value, std::string* out) {
  out->push_back('"');
  for (const char c : value) {
    switch (c) {
      case '"': out->append("\\\""); break;
      case '\\': out->append("\\\\"); break;
      case '\b': out->append("\\b"); break;
      case '\f': out->append("\\f"); break;
      case '\n': out->append("\\n"); break;
      case '\r': out->append("\\r"); break;
      case '\t': out->append("\\t"); break;
      default:
        if (static_cast<unsigned char>(c) < 0x20) {
          char buffer[8];
          std::snprintf(buffer, sizeof(buffer), "\\u%04x",
                        static_cast<unsigned int>(static_cast<unsigned char>(c)));
          out->append(buffer);
        } else {
          out->push_back(c);
        }
    }
  }
  out->push_back('"');
}

void dump_value(const Json& value, int indent, std::string* out) {
  const std::string pad(static_cast<size_t>(indent), ' ');
  const std::string inner_pad(static_cast<size_t>(indent + 2), ' ');
  switch (value.type()) {
    case Json::Type::null: out->append("null"); return;
    case Json::Type::boolean: out->append(value.as_bool() ? "true" : "false"); return;
    case Json::Type::integer: out->append(std::to_string(value.as_integer())); return;
    case Json::Type::number: {
      if (!std::isfinite(value.as_number())) {
        // Non-finite values would produce invalid JSON; use the null literal
        // and let callers treat null as "not measurable".
        out->append("null");
        return;
      }
      char buffer[64];
      const auto result = std::to_chars(buffer, buffer + sizeof(buffer),
                                        value.as_number());
      std::string text(buffer, result.ptr);
      // Keep the token visibly floating point so a round trip preserves type.
      if (text.find('.') == std::string::npos &&
          text.find('e') == std::string::npos &&
          text.find('n') == std::string::npos) {
        text.append(".0");
      }
      out->append(text);
      return;
    }
    case Json::Type::string: dump_string(value.as_string(), out); return;
    case Json::Type::array: {
      if (value.items().empty()) {
        out->append("[]");
        return;
      }
      out->append("[\n");
      for (size_t i = 0; i < value.items().size(); ++i) {
        out->append(inner_pad);
        dump_value(value.items()[i], indent + 2, out);
        if (i + 1 != value.items().size()) out->push_back(',');
        out->push_back('\n');
      }
      out->append(pad);
      out->push_back(']');
      return;
    }
    case Json::Type::object: {
      if (value.members().empty()) {
        out->append("{}");
        return;
      }
      out->append("{\n");
      for (size_t i = 0; i < value.members().size(); ++i) {
        out->append(inner_pad);
        dump_string(value.members()[i].first, out);
        out->append(": ");
        dump_value(value.members()[i].second, indent + 2, out);
        if (i + 1 != value.members().size()) out->push_back(',');
        out->push_back('\n');
      }
      out->append(pad);
      out->push_back('}');
      return;
    }
  }
}

}  // namespace

Json::Json() = default;
Json::Json(bool value) : type_(Type::boolean), bool_(value) {}
Json::Json(int64_t value) : type_(Type::integer), integer_(value) {}
Json::Json(double value) : type_(Type::number), number_(value) {}
Json::Json(std::string value) : type_(Type::string), string_(std::move(value)) {}
Json::Json(const char* value)
    : type_(Type::string), string_(value == nullptr ? "" : value) {}

Json Json::array() {
  Json json;
  json.type_ = Type::array;
  return json;
}

Json Json::object() {
  Json json;
  json.type_ = Type::object;
  return json;
}

double Json::as_number() const {
  return type_ == Type::integer ? static_cast<double>(integer_) : number_;
}

void Json::push(Json value) {
  if (type_ == Type::array) items_.push_back(std::move(value));
}

Json& Json::set(std::string key, Json value) {
  if (type_ != Type::object) return *this;
  if (Json* existing = find(key)) {
    *existing = std::move(value);
    return *existing;
  }
  members_.emplace_back(std::move(key), std::move(value));
  return members_.back().second;
}

const Json* Json::find(std::string_view key) const {
  if (type_ != Type::object) return nullptr;
  for (const auto& member : members_) {
    if (member.first == key) return &member.second;
  }
  return nullptr;
}

Json* Json::find(std::string_view key) {
  if (type_ != Type::object) return nullptr;
  for (auto& member : members_) {
    if (member.first == key) return &member.second;
  }
  return nullptr;
}

Json Json::parse(std::string_view text, std::string* error, size_t* error_offset) {
  Parser parser;
  parser.text = text;
  Json result;
  if (!parser.parse_value(&result, 0)) {
    if (error) *error = parser.error_;
    if (error_offset) *error_offset = parser.error_pos_;
    return Json();
  }
  parser.skip_ws();
  if (!parser.eof()) {
    if (error) {
      *error = "trailing characters at byte " + std::to_string(parser.pos);
    }
    if (error_offset) *error_offset = parser.pos;
    return Json();
  }
  return result;
}

std::string Json::dump() const {
  std::string out;
  dump_value(*this, 0, &out);
  out.push_back('\n');
  return out;
}

bool Json::operator==(const Json& other) const {
  if (type_ != other.type_) return false;
  switch (type_) {
    case Type::null: return true;
    case Type::boolean: return bool_ == other.bool_;
    case Type::integer: return integer_ == other.integer_;
    case Type::number: return number_ == other.number_;
    case Type::string: return string_ == other.string_;
    case Type::array: return items_ == other.items_;
    case Type::object: return members_ == other.members_;
  }
  return false;
}

}  // namespace media_restore
