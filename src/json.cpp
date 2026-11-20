#include "relay/json.hpp"

#include <algorithm>
#include <charconv>
#include <cmath>
#include <cstdio>

namespace relay::json {
namespace {

class Parser {
 public:
  Parser(std::string_view text, ParseError& error) : text_(text), error_(error) {}

  bool run(Value& out) {
    skip_space();
    if (!parse_value(out)) {
      return false;
    }
    skip_space();
    if (pos_ != text_.size()) {
      return fail("trailing characters after JSON value");
    }
    return true;
  }

 private:
  bool fail(std::string message) {
    if (error_.message.empty()) {
      error_.offset = pos_;
      error_.message = std::move(message);
    }
    return false;
  }

  void skip_space() {
    while (pos_ < text_.size()) {
      const char c = text_[pos_];
      if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
        ++pos_;
      } else {
        break;
      }
    }
  }

  bool parse_value(Value& out) {
    if (pos_ >= text_.size()) {
      return fail("unexpected end of input");
    }
    switch (text_[pos_]) {
      case '{':
        return parse_object(out);
      case '[':
        return parse_array(out);
      case '"': {
        std::string value;
        if (!parse_string(value)) {
          return false;
        }
        out = Value(std::move(value));
        return true;
      }
      case 't':
        return literal("true", Value(true), out);
      case 'f':
        return literal("false", Value(false), out);
      case 'n':
        return literal("null", Value(), out);
      default:
        return parse_number(out);
    }
  }

  bool literal(std::string_view word, Value value, Value& out) {
    if (text_.substr(pos_, word.size()) != word) {
      return fail("invalid literal");
    }
    pos_ += word.size();
    out = std::move(value);
    return true;
  }

  bool parse_number(Value& out) {
    const std::size_t begin = pos_;
    if (pos_ < text_.size() && (text_[pos_] == '-' || text_[pos_] == '+')) {
      ++pos_;
    }
    bool digits = false;
    while (pos_ < text_.size() &&
           ((text_[pos_] >= '0' && text_[pos_] <= '9') || text_[pos_] == '.' ||
            text_[pos_] == 'e' || text_[pos_] == 'E' || text_[pos_] == '+' ||
            text_[pos_] == '-')) {
      if (text_[pos_] >= '0' && text_[pos_] <= '9') {
        digits = true;
      }
      ++pos_;
    }
    if (!digits) {
      return fail("invalid number");
    }
    const std::string token(text_.substr(begin, pos_ - begin));
    double value = 0.0;
    try {
      value = std::stod(token);
    } catch (const std::exception&) {
      return fail("number out of range");
    }
    out = Value(value);
    return true;
  }

  bool parse_string(std::string& out) {
    if (text_[pos_] != '"') {
      return fail("expected string");
    }
    ++pos_;
    while (pos_ < text_.size()) {
      const char c = text_[pos_++];
      if (c == '"') {
        return true;
      }
      if (c == '\\') {
        if (pos_ >= text_.size()) {
          return fail("unterminated escape");
        }
        const char escape = text_[pos_++];
        switch (escape) {
          case '"': out.push_back('"'); break;
          case '\\': out.push_back('\\'); break;
          case '/': out.push_back('/'); break;
          case 'b': out.push_back('\b'); break;
          case 'f': out.push_back('\f'); break;
          case 'n': out.push_back('\n'); break;
          case 'r': out.push_back('\r'); break;
          case 't': out.push_back('\t'); break;
          case 'u': {
            if (pos_ + 4 > text_.size()) {
              return fail("truncated \\u escape");
            }
            unsigned code = 0;
            const auto [ptr, ec] =
                std::from_chars(text_.data() + pos_, text_.data() + pos_ + 4,
                                code, 16);
            if (ec != std::errc{} || ptr != text_.data() + pos_ + 4) {
              return fail("invalid \\u escape");
            }
            pos_ += 4;
            // Encode as UTF-8; surrogate pairs are outside this format's
            // needs (identifiers are ASCII) but are rejected explicitly.
            if (code >= 0xd800 && code <= 0xdfff) {
              return fail("surrogate escapes are not supported");
            }
            if (code < 0x80) {
              out.push_back(static_cast<char>(code));
            } else if (code < 0x800) {
              out.push_back(static_cast<char>(0xc0 | (code >> 6)));
              out.push_back(static_cast<char>(0x80 | (code & 0x3f)));
            } else {
              out.push_back(static_cast<char>(0xe0 | (code >> 12)));
              out.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3f)));
              out.push_back(static_cast<char>(0x80 | (code & 0x3f)));
            }
            break;
          }
          default:
            return fail("unsupported escape");
        }
        continue;
      }
      if (static_cast<unsigned char>(c) < 0x20) {
        return fail("control character in string");
      }
      out.push_back(c);
    }
    return fail("unterminated string");
  }

  bool parse_array(Value& out) {
    ++pos_;  // '['
    Array array;
    skip_space();
    if (pos_ < text_.size() && text_[pos_] == ']') {
      ++pos_;
      out = Value(std::move(array));
      return true;
    }
    while (true) {
      Value element;
      skip_space();
      if (!parse_value(element)) {
        return false;
      }
      array.push_back(std::move(element));
      skip_space();
      if (pos_ >= text_.size()) {
        return fail("unterminated array");
      }
      if (text_[pos_] == ',') {
        ++pos_;
        skip_space();
        if (pos_ < text_.size() && text_[pos_] == ']') {
          return fail("trailing comma in array");
        }
        continue;
      }
      if (text_[pos_] == ']') {
        ++pos_;
        out = Value(std::move(array));
        return true;
      }
      return fail("expected ',' or ']' in array");
    }
  }

  bool parse_object(Value& out) {
    ++pos_;  // '{'
    Object object;
    skip_space();
    if (pos_ < text_.size() && text_[pos_] == '}') {
      ++pos_;
      out = Value(std::move(object));
      return true;
    }
    while (true) {
      skip_space();
      std::string key;
      if (pos_ >= text_.size() || text_[pos_] != '"') {
        return fail("object key must be a string");
      }
      if (!parse_string(key)) {
        return false;
      }
      skip_space();
      if (pos_ >= text_.size() || text_[pos_] != ':') {
        return fail("expected ':' after object key");
      }
      ++pos_;
      skip_space();
      Value value;
      if (!parse_value(value)) {
        return false;
      }
      object.insert_or_assign(std::move(key), std::move(value));
      skip_space();
      if (pos_ >= text_.size()) {
        return fail("unterminated object");
      }
      if (text_[pos_] == ',') {
        ++pos_;
        skip_space();
        if (pos_ < text_.size() && text_[pos_] == '}') {
          return fail("trailing comma in object");
        }
        continue;
      }
      if (text_[pos_] == '}') {
        ++pos_;
        out = Value(std::move(object));
        return true;
      }
      return fail("expected ',' or '}' in object");
    }
  }

  std::string_view text_;
  ParseError& error_;
  std::size_t pos_ = 0;
};

void dump_string(const std::string& value, std::string& out) {
  out.push_back('"');
  for (const char c : value) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (static_cast<unsigned char>(c) < 0x20) {
          char buffer[8];
          std::snprintf(buffer, sizeof(buffer), "\\u%04x",
                        static_cast<unsigned>(static_cast<unsigned char>(c)));
          out += buffer;
        } else {
          out.push_back(c);
        }
    }
  }
  out.push_back('"');
}

void dump_number(double value, std::string& out) {
  if (std::isfinite(value) && value == std::floor(value) &&
      std::abs(value) < 9.2e18) {
    out += std::to_string(static_cast<std::int64_t>(value));
    return;
  }
  char buffer[32];
  std::snprintf(buffer, sizeof(buffer), "%.17g", value);
  out += buffer;
}

}  // namespace

const Value* Value::find(std::string_view key) const {
  if (type_ != Type::kObject) {
    return nullptr;
  }
  auto it = object_.find(std::string(key));
  return it == object_.end() ? nullptr : &it->second;
}

Value& Value::operator[](const std::string& key) {
  type_ = Type::kObject;
  return object_[key];
}

void Value::push(Value value) {
  type_ = Type::kArray;
  array_.push_back(std::move(value));
}

bool Value::contains(std::string_view key) const {
  return find(key) != nullptr;
}

std::string Value::dump(int indent) const {
  std::string out;
  dump_into(out, indent, 0);
  return out;
}

void Value::dump_into(std::string& out, int indent, int depth) const {
  const std::string pad(static_cast<std::size_t>(indent * (depth + 1)), ' ');
  const std::string close_pad(static_cast<std::size_t>(indent * depth), ' ');
  switch (type_) {
    case Type::kNull: out += "null"; return;
    case Type::kBool: out += bool_ ? "true" : "false"; return;
    case Type::kNumber: dump_number(number_, out); return;
    case Type::kString: dump_string(string_, out); return;
    case Type::kArray: {
      if (array_.empty()) {
        out += "[]";
        return;
      }
      const bool inline_array =
          indent == 0 || std::all_of(array_.begin(), array_.end(), [](const Value& v) {
            return v.type_ != Type::kObject && v.type_ != Type::kArray;
          });
      out += inline_array ? "[" : "[\n";
      for (std::size_t i = 0; i < array_.size(); ++i) {
        if (i != 0) {
          out += inline_array ? ", " : ",\n";
        }
        if (!inline_array) {
          out += pad;
        }
        array_[i].dump_into(out, indent, depth + 1);
      }
      if (inline_array) {
        out += ']';
      } else {
        out += '\n';
        out += close_pad;
        out += ']';
      }
      return;
    }
    case Type::kObject: {
      if (object_.empty()) {
        out += "{}";
        return;
      }
      out += "{\n";
      std::size_t index = 0;
      for (const auto& [key, value] : object_) {
        if (index++ != 0) {
          out += ",\n";
        }
        out += pad;
        dump_string(key, out);
        out += ": ";
        value.dump_into(out, indent, depth + 1);
      }
      out += '\n';
      out += close_pad;
      out += '}';
      return;
    }
  }
}

bool parse(std::string_view text, Value& out, ParseError& error) {
  Parser parser(text, error);
  return parser.run(out);
}

}  // namespace relay::json
