#include "media/json.hpp"

#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <string>
#include <utility>

namespace mediaqc {
namespace {

void append_escaped(std::string& out, std::string_view text) {
  out.push_back('"');
  for (unsigned char c : text) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\b': out += "\\b"; break;
      case '\f': out += "\\f"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (c < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof buf, "\\u%04x", c);
          out += buf;
        } else {
          out.push_back(static_cast<char>(c));
        }
    }
  }
  out.push_back('"');
}

void dump_value(const Json& value, bool pretty, int depth, std::string& out);

void indent(std::string& out, int depth) {
  out.append(static_cast<std::size_t>(depth) * 2, ' ');
}

void dump_array(const Json& value, bool pretty, int depth, std::string& out) {
  const auto& items = value.items();
  if (items.empty()) {
    out += "[]";
    return;
  }
  out += '[';
  for (std::size_t i = 0; i < items.size(); ++i) {
    if (i != 0) {
      out += ',';
    }
    if (pretty) {
      out += '\n';
      indent(out, depth + 1);
    }
    dump_value(items[i], pretty, depth + 1, out);
  }
  if (pretty) {
    out += '\n';
    indent(out, depth);
  }
  out += ']';
}

void dump_object(const Json& value, bool pretty, int depth, std::string& out) {
  const auto& members = value.members();
  if (members.empty()) {
    out += "{}";
    return;
  }
  out += '{';
  bool first = true;
  for (const auto& [key, child] : members) {
    if (!first) {
      out += ',';
    }
    first = false;
    if (pretty) {
      out += '\n';
      indent(out, depth + 1);
    }
    append_escaped(out, key);
    out += ':';
    if (pretty) {
      out += ' ';
    }
    dump_value(child, pretty, depth + 1, out);
  }
  if (pretty) {
    out += '\n';
    indent(out, depth);
  }
  out += '}';
}

void dump_value(const Json& value, bool pretty, int depth, std::string& out) {
  switch (value.type()) {
    case Json::Type::kNull:
      out += "null";
      break;
    case Json::Type::kBool:
      out += value.as_bool() ? "true" : "false";
      break;
    case Json::Type::kInt:
      out += std::to_string(value.as_int());
      break;
    case Json::Type::kDouble: {
      char buf[64];
      // Round-trippable and locale independent.
      std::snprintf(buf, sizeof buf, "%.17g", value.as_double());
      out += buf;
      break;
    }
    case Json::Type::kString:
      append_escaped(out, value.as_string());
      break;
    case Json::Type::kArray:
      dump_array(value, pretty, depth, out);
      break;
    case Json::Type::kObject:
      dump_object(value, pretty, depth, out);
      break;
  }
}

class Parser {
 public:
  explicit Parser(std::string_view text) : text_(text) {}

  JsonParseResult parse() {
    JsonParseResult result;
    skip_space();
    result.value = parse_value(result);
    if (!result.error.empty()) {
      result.error_at = pos_;
      return result;
    }
    skip_space();
    if (pos_ != text_.size()) {
      fail(result, "trailing characters after JSON value");
      result.error_at = pos_;
      return result;
    }
    result.ok = true;
    return result;
  }

 private:
  void skip_space() {
    while (pos_ < text_.size()) {
      char c = text_[pos_];
      if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
        ++pos_;
      } else {
        break;
      }
    }
  }

  void fail(JsonParseResult& result, std::string message) {
    if (result.error.empty()) {
      result.error = std::move(message);
      result.error_at = pos_;
    }
    pos_ = text_.size();
  }

  bool consume(std::string_view token) {
    if (text_.substr(pos_, token.size()) == token) {
      pos_ += token.size();
      return true;
    }
    return false;
  }

  Json parse_value(JsonParseResult& result) {
    if (pos_ >= text_.size()) {
      fail(result, "unexpected end of JSON");
      return Json{};
    }
    char c = text_[pos_];
    switch (c) {
      case '{': return parse_object(result);
      case '[': return parse_array(result);
      case '"': return parse_string(result);
      case 't':
        if (consume("true")) return Json::make_bool(true);
        fail(result, "invalid literal");
        return Json{};
      case 'f':
        if (consume("false")) return Json::make_bool(false);
        fail(result, "invalid literal");
        return Json{};
      case 'n':
        if (consume("null")) return Json::make_null();
        fail(result, "invalid literal");
        return Json{};
      default:
        if (c == '-' || (c >= '0' && c <= '9')) {
          return parse_number(result);
        }
        fail(result, "unexpected character");
        return Json{};
    }
  }

  Json parse_object(JsonParseResult& result) {
    ++pos_;  // '{'
    Json obj = Json::make_object();
    skip_space();
    if (pos_ < text_.size() && text_[pos_] == '}') {
      ++pos_;
      return obj;
    }
    while (true) {
      skip_space();
      if (pos_ >= text_.size() || text_[pos_] != '"') {
        fail(result, "expected object key string");
        return Json{};
      }
      Json key = parse_string(result);
      if (!result.error.empty()) return Json{};
      skip_space();
      if (pos_ >= text_.size() || text_[pos_] != ':') {
        fail(result, "expected ':' after object key");
        return Json{};
      }
      ++pos_;
      skip_space();
      Json value = parse_value(result);
      if (!result.error.empty()) return Json{};
      obj.set(key.as_string(), std::move(value));
      skip_space();
      if (pos_ >= text_.size()) {
        fail(result, "unterminated object");
        return Json{};
      }
      if (text_[pos_] == ',') {
        ++pos_;
        continue;
      }
      if (text_[pos_] == '}') {
        ++pos_;
        return obj;
      }
      fail(result, "expected ',' or '}' in object");
      return Json{};
    }
  }

  Json parse_array(JsonParseResult& result) {
    ++pos_;  // '['
    Json arr = Json::make_array();
    skip_space();
    if (pos_ < text_.size() && text_[pos_] == ']') {
      ++pos_;
      return arr;
    }
    while (true) {
      skip_space();
      Json value = parse_value(result);
      if (!result.error.empty()) return Json{};
      arr.push(std::move(value));
      skip_space();
      if (pos_ >= text_.size()) {
        fail(result, "unterminated array");
        return Json{};
      }
      if (text_[pos_] == ',') {
        ++pos_;
        continue;
      }
      if (text_[pos_] == ']') {
        ++pos_;
        return arr;
      }
      fail(result, "expected ',' or ']' in array");
      return Json{};
    }
  }

  Json parse_string(JsonParseResult& result) {
    ++pos_;  // '"'
    std::string out;
    while (pos_ < text_.size()) {
      char c = text_[pos_++];
      if (c == '"') {
        return Json::make_string(std::move(out));
      }
      if (static_cast<unsigned char>(c) < 0x20) {
        fail(result, "control character in string");
        return Json{};
      }
      if (c != '\\') {
        out.push_back(c);
        continue;
      }
      if (pos_ >= text_.size()) {
        fail(result, "unterminated escape");
        return Json{};
      }
      char esc = text_[pos_++];
      switch (esc) {
        case '"': out.push_back('"'); break;
        case '\\': out.push_back('\\'); break;
        case '/': out.push_back('/'); break;
        case 'b': out.push_back('\b'); break;
        case 'f': out.push_back('\f'); break;
        case 'n': out.push_back('\n'); break;
        case 'r': out.push_back('\r'); break;
        case 't': out.push_back('\t'); break;
        case 'u': {
          if (!parse_unicode_escape(out, result)) {
            return Json{};
          }
          break;
        }
        default:
          fail(result, "invalid escape");
          return Json{};
      }
    }
    fail(result, "unterminated string");
    return Json{};
  }

  bool parse_unicode_escape(std::string& out, JsonParseResult& result) {
    std::uint32_t cp = 0;
    if (!read_hex4(cp)) {
      fail(result, "invalid \\u escape");
      return false;
    }
    if (cp >= 0xD800 && cp <= 0xDBFF) {
      // High surrogate: require a following low surrogate.
      if (pos_ + 1 >= text_.size() || text_[pos_] != '\\' || text_[pos_ + 1] != 'u') {
        fail(result, "unpaired surrogate");
        return false;
      }
      pos_ += 2;
      std::uint32_t low = 0;
      if (!read_hex4(low) || low < 0xDC00 || low > 0xDFFF) {
        fail(result, "invalid low surrogate");
        return false;
      }
      cp = 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00);
    } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
      fail(result, "unpaired low surrogate");
      return false;
    }
    append_utf8(out, cp);
    return true;
  }

  bool read_hex4(std::uint32_t& cp) {
    if (pos_ + 4 > text_.size()) return false;
    cp = 0;
    for (int i = 0; i < 4; ++i) {
      char c = text_[pos_++];
      cp <<= 4;
      if (c >= '0' && c <= '9') cp |= static_cast<std::uint32_t>(c - '0');
      else if (c >= 'a' && c <= 'f') cp |= static_cast<std::uint32_t>(c - 'a' + 10);
      else if (c >= 'A' && c <= 'F') cp |= static_cast<std::uint32_t>(c - 'A' + 10);
      else return false;
    }
    return true;
  }

  static void append_utf8(std::string& out, std::uint32_t cp) {
    if (cp < 0x80) {
      out.push_back(static_cast<char>(cp));
    } else if (cp < 0x800) {
      out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
      out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp < 0x10000) {
      out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
      out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else {
      out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
      out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    }
  }

  Json parse_number(JsonParseResult& result) {
    std::size_t start = pos_;
    bool is_double = false;
    if (pos_ < text_.size() && text_[pos_] == '-') ++pos_;
    while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
    if (pos_ < text_.size() && text_[pos_] == '.') {
      is_double = true;
      ++pos_;
      while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
    }
    if (pos_ < text_.size() && (text_[pos_] == 'e' || text_[pos_] == 'E')) {
      is_double = true;
      ++pos_;
      if (pos_ < text_.size() && (text_[pos_] == '+' || text_[pos_] == '-')) ++pos_;
      while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
    }
    std::string token(text_.substr(start, pos_ - start));
    if (token.empty() || token == "-") {
      fail(result, "invalid number");
      return Json{};
    }
    if (!is_double) {
      char* end = nullptr;
      errno = 0;
      long long value = std::strtoll(token.c_str(), &end, 10);
      if (errno == 0 && end && *end == '\0') {
        return Json::make_int(static_cast<std::int64_t>(value));
      }
      // Integer overflow falls through to a double representation.
    }
    char* end = nullptr;
    double value = std::strtod(token.c_str(), &end);
    if (!end || *end != '\0') {
      fail(result, "invalid number");
      return Json{};
    }
    return Json::make_double(value);
  }

  std::string_view text_;
  std::size_t pos_ = 0;
};

}  // namespace

Json Json::make_null() { return Json{}; }

Json Json::make_bool(bool value) {
  Json j;
  j.type_ = Type::kBool;
  j.bool_ = value;
  return j;
}

Json Json::make_int(std::int64_t value) {
  Json j;
  j.type_ = Type::kInt;
  j.int_ = value;
  return j;
}

Json Json::make_double(double value) {
  Json j;
  j.type_ = Type::kDouble;
  j.double_ = value;
  return j;
}

Json Json::make_string(std::string value) {
  Json j;
  j.type_ = Type::kString;
  j.string_ = std::move(value);
  return j;
}

Json Json::make_array(std::vector<Json> values) {
  Json j;
  j.type_ = Type::kArray;
  j.array_ = std::move(values);
  return j;
}

Json Json::make_object(std::map<std::string, Json, std::less<>> values) {
  Json j;
  j.type_ = Type::kObject;
  j.object_ = std::move(values);
  return j;
}

bool Json::as_bool(bool fallback) const {
  return type_ == Type::kBool ? bool_ : fallback;
}

std::int64_t Json::as_int(std::int64_t fallback) const {
  if (type_ == Type::kInt) return int_;
  if (type_ == Type::kDouble) return static_cast<std::int64_t>(double_);
  return fallback;
}

double Json::as_double(double fallback) const {
  if (type_ == Type::kDouble) return double_;
  if (type_ == Type::kInt) return static_cast<double>(int_);
  return fallback;
}

const std::string& Json::as_string() const {
  static const std::string empty;
  return type_ == Type::kString ? string_ : empty;
}

const Json* Json::find(std::string_view key) const {
  if (type_ != Type::kObject) return nullptr;
  auto it = object_.find(key);
  return it == object_.end() ? nullptr : &it->second;
}

Json& Json::operator[](std::string key) {
  if (type_ != Type::kObject) {
    type_ = Type::kObject;
  }
  return object_[std::move(key)];
}

void Json::set(std::string key, Json value) {
  if (type_ != Type::kObject) {
    type_ = Type::kObject;
  }
  object_.insert_or_assign(std::move(key), std::move(value));
}

void Json::push(Json value) {
  if (type_ != Type::kArray) {
    type_ = Type::kArray;
  }
  array_.push_back(std::move(value));
}

std::size_t Json::size() const {
  switch (type_) {
    case Type::kArray: return array_.size();
    case Type::kObject: return object_.size();
    case Type::kString: return string_.size();
    default: return 0;
  }
}

JsonParseResult parse_json(std::string_view text) {
  Parser parser(text);
  return parser.parse();
}

std::string dump_json(const Json& value, bool pretty) {
  std::string out;
  dump_value(value, pretty, 0, out);
  return out;
}

}  // namespace mediaqc
