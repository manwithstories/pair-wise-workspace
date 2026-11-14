#include "json.h"

#include <cctype>

#include "bitreader.h"

namespace h264 {

const JsonValue* JsonValue::find(const std::string& key) const {
  if (type != Type::kObj) {
    return nullptr;
  }
  auto it = obj.find(key);
  return it == obj.end() ? nullptr : &it->second;
}

namespace {

class Parser {
public:
  explicit Parser(const std::string& s) : s_(s) {}

  JsonValue run() {
    ws();
    JsonValue v = value();
    ws();
    if (pos_ != s_.size()) {
      fail("trailing characters after JSON document");
    }
    return v;
  }

private:
  [[noreturn]] void fail(const std::string& msg) {
    fail_parse("json: " + msg + " at offset " + std::to_string(pos_));
  }

  void ws() {
    while (pos_ < s_.size() && isspace(static_cast<unsigned char>(s_[pos_]))) {
      ++pos_;
    }
  }

  char peek() {
    if (pos_ >= s_.size()) {
      fail("unexpected end of input");
    }
    return s_[pos_];
  }

  void expect(char c) {
    if (peek() != c) {
      fail(std::string("expected '") + c + "'");
    }
    ++pos_;
  }

  JsonValue value() {
    char c = peek();
    if (c == '{') return object();
    if (c == '[') return array();
    if (c == '"') return string_value();
    if (c == 't' || c == 'f') return boolean();
    if (c == 'n') return null();
    return number();
  }

  JsonValue object() {
    JsonValue v;
    v.type = JsonValue::Type::kObj;
    expect('{');
    ws();
    if (peek() == '}') {
      ++pos_;
      return v;
    }
    while (true) {
      ws();
      JsonValue key = string_value();
      ws();
      expect(':');
      ws();
      v.obj[key.str] = value();
      ws();
      char c = peek();
      ++pos_;
      if (c == '}') {
        return v;
      }
      if (c != ',') {
        fail("expected ',' or '}'");
      }
    }
  }

  JsonValue array() {
    JsonValue v;
    v.type = JsonValue::Type::kArr;
    expect('[');
    ws();
    if (peek() == ']') {
      ++pos_;
      return v;
    }
    while (true) {
      ws();
      v.arr.push_back(value());
      ws();
      char c = peek();
      ++pos_;
      if (c == ']') {
        return v;
      }
      if (c != ',') {
        fail("expected ',' or ']'");
      }
    }
  }

  JsonValue string_value() {
    JsonValue v;
    v.type = JsonValue::Type::kStr;
    expect('"');
    while (true) {
      char c = peek();
      ++pos_;
      if (c == '"') {
        return v;
      }
      if (c == '\\') {
        char e = peek();
        ++pos_;
        switch (e) {
          case 'n': v.str += '\n'; break;
          case 't': v.str += '\t'; break;
          case 'r': v.str += '\r'; break;
          case 'u':
            for (int i = 0; i < 4; ++i) ++pos_;
            v.str += '?';
            break;
          default: v.str += e; break;
        }
      } else {
        v.str += c;
      }
    }
  }

  JsonValue number() {
    size_t start = pos_;
    if (peek() == '-') ++pos_;
    while (pos_ < s_.size() &&
           (isdigit(static_cast<unsigned char>(s_[pos_])) || s_[pos_] == '.' ||
            s_[pos_] == 'e' || s_[pos_] == 'E' || s_[pos_] == '+' || s_[pos_] == '-')) {
      ++pos_;
    }
    if (pos_ == start) {
      fail("invalid value");
    }
    JsonValue v;
    v.type = JsonValue::Type::kNum;
    try {
      v.number = std::stod(s_.substr(start, pos_ - start));
    } catch (...) {
      fail("invalid number");
    }
    return v;
  }

  JsonValue boolean() {
    JsonValue v;
    v.type = JsonValue::Type::kBool;
    if (s_.compare(pos_, 4, "true") == 0) {
      v.boolean = true;
      pos_ += 4;
    } else if (s_.compare(pos_, 5, "false") == 0) {
      v.boolean = false;
      pos_ += 5;
    } else {
      fail("invalid literal");
    }
    return v;
  }

  JsonValue null() {
    if (s_.compare(pos_, 4, "null") != 0) {
      fail("invalid literal");
    }
    pos_ += 4;
    return JsonValue{};
  }

  const std::string& s_;
  size_t pos_ = 0;
};

}  // namespace

JsonValue json_parse(const std::string& text) { return Parser(text).run(); }

}  // namespace h264
