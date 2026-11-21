#include "media/json.hpp"

#include <charconv>
#include <sstream>

namespace media::json {
namespace {

class Parser {
 public:
  Parser(std::string_view text, std::string* error) : t_(text), err_(error) {}

  Value parseRoot() {
    skipWs();
    Value v = parseValue();
    skipWs();
    if (!failed_ && pos_ != t_.size()) fail("trailing characters");
    return v;
  }

 private:
  std::string_view t_;
  std::string* err_;
  size_t pos_ = 0;
  bool failed_ = false;

  void fail(const std::string& msg) {
    if (!failed_) {
      failed_ = true;
      if (err_) *err_ = msg + " at offset " + std::to_string(pos_);
    }
  }
  void skipWs() {
    while (pos_ < t_.size() &&
           (t_[pos_] == ' ' || t_[pos_] == '\t' || t_[pos_] == '\n' || t_[pos_] == '\r'))
      ++pos_;
  }
  char peek() { return pos_ < t_.size() ? t_[pos_] : '\0'; }

  Value parseValue() {
    if (failed_) return {};
    skipWs();
    char c = peek();
    switch (c) {
      case '{': return parseObject();
      case '[': return parseArray();
      case '"': return Value::makeString(parseString());
      case 't': case 'f': return parseBool();
      case 'n': return parseNull();
      default:
        if (c == '-' || (c >= '0' && c <= '9')) return parseNumber();
        fail("unexpected character");
        return {};
    }
  }

  Value parseObject() {
    Value obj = Value::makeObject();
    ++pos_;
    skipWs();
    if (peek() == '}') { ++pos_; return obj; }
    while (!failed_) {
      skipWs();
      if (peek() != '"') { fail("expected key"); break; }
      std::string key = parseString();
      skipWs();
      if (peek() != ':') { fail("expected ':'"); break; }
      ++pos_;
      obj.set(key, parseValue());
      skipWs();
      char d = peek();
      if (d == ',') { ++pos_; continue; }
      if (d == '}') { ++pos_; return obj; }
      fail("expected ',' or '}'");
    }
    return {};
  }

  Value parseArray() {
    Value arr = Value::makeArray();
    ++pos_;
    skipWs();
    if (peek() == ']') { ++pos_; return arr; }
    while (!failed_) {
      arr.push(parseValue());
      skipWs();
      char d = peek();
      if (d == ',') { ++pos_; continue; }
      if (d == ']') { ++pos_; return arr; }
      fail("expected ',' or ']'");
    }
    return {};
  }

  std::string parseString() {
    std::string out;
    ++pos_;  // opening quote
    while (pos_ < t_.size()) {
      char c = t_[pos_++];
      if (c == '"') return out;
      if (c == '\\') {
        if (pos_ >= t_.size()) break;
        char e = t_[pos_++];
        switch (e) {
          case '"': out.push_back('"'); break;
          case '\\': out.push_back('\\'); break;
          case '/': out.push_back('/'); break;
          case 'b': out.push_back('\b'); break;
          case 'f': out.push_back('\f'); break;
          case 'n': out.push_back('\n'); break;
          case 'r': out.push_back('\r'); break;
          case 't': out.push_back('\t'); break;
          case 'u': {
            if (pos_ + 4 > t_.size()) { fail("bad unicode escape"); return out; }
            unsigned code = 0;
            auto [p, ec] = std::from_chars(t_.data() + pos_, t_.data() + pos_ + 4,
                                           code, 16);
            (void)p;
            if (ec != std::errc{}) { fail("bad unicode escape"); return out; }
            pos_ += 4;
            if (code < 0x80) {
              out.push_back(static_cast<char>(code));
            } else if (code < 0x800) {
              out.push_back(static_cast<char>(0xC0 | (code >> 6)));
              out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
            } else {
              out.push_back(static_cast<char>(0xE0 | (code >> 12)));
              out.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
              out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
            }
            break;
          }
          default: fail("bad escape"); return out;
        }
      } else {
        out.push_back(c);
      }
    }
    fail("unterminated string");
    return out;
  }

  Value parseBool() {
    if (t_.compare(pos_, 4, "true") == 0) { pos_ += 4; return Value::makeBool(true); }
    if (t_.compare(pos_, 5, "false") == 0) { pos_ += 5; return Value::makeBool(false); }
    fail("bad literal");
    return {};
  }

  Value parseNull() {
    if (t_.compare(pos_, 4, "null") == 0) { pos_ += 4; return {}; }
    fail("bad literal");
    return {};
  }

  Value parseNumber() {
    size_t start = pos_;
    if (peek() == '-') ++pos_;
    while (pos_ < t_.size() && t_[pos_] >= '0' && t_[pos_] <= '9') ++pos_;
    // 仅支持整数；小数/指数不是本项目产出，但需安全拒绝。
    if (peek() == '.' || peek() == 'e' || peek() == 'E') { fail("number not integer"); return {}; }
    int64_t n = 0;
    auto [p, ec] = std::from_chars(t_.data() + start, t_.data() + pos_, n);
    if (ec != std::errc{} || p != t_.data() + pos_) { fail("bad number"); return {}; }
    return Value::makeNumber(n);
  }
};

void dumpTo(const Value& v, int indent, int depth, std::ostringstream& os);

void dumpString(const std::string& s, std::ostringstream& os) {
  os << '"';
  for (char c : s) {
    switch (c) {
      case '"': os << "\\\""; break;
      case '\\': os << "\\\\"; break;
      case '\b': os << "\\b"; break;
      case '\f': os << "\\f"; break;
      case '\n': os << "\\n"; break;
      case '\r': os << "\\r"; break;
      case '\t': os << "\\t"; break;
      default:
        if (static_cast<unsigned char>(c) < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof(buf), "\\u%04x", c);
          os << buf;
        } else {
          os << c;
        }
    }
  }
  os << '"';
}

void pad(int indent, int depth, std::ostringstream& os) {
  if (indent <= 0) return;
  os << '\n' << std::string(static_cast<size_t>(indent * depth), ' ');
}

void dumpTo(const Value& v, int indent, int depth, std::ostringstream& os) {
  switch (v.type()) {
    case Value::Type::Null: os << "null"; break;
    case Value::Type::Bool: os << (v.asBool() ? "true" : "false"); break;
    case Value::Type::Number: os << v.asNumber(); break;
    case Value::Type::String: dumpString(v.asString(), os); break;
    case Value::Type::Array: {
      const auto& a = v.asArray();
      if (a.empty()) { os << "[]"; break; }
      os << '[';
      for (size_t i = 0; i < a.size(); ++i) {
        if (i) os << ',';
        pad(indent, depth + 1, os);
        dumpTo(a[i], indent, depth + 1, os);
      }
      pad(indent, depth, os);
      os << ']';
      break;
    }
    case Value::Type::Object: {
      const auto& o = v.asObject();
      if (o.empty()) { os << "{}"; break; }
      os << '{';
      size_t i = 0;
      for (const auto& [k, child] : o) {
        if (i++) os << ',';
        pad(indent, depth + 1, os);
        dumpString(k, os);
        os << (indent > 0 ? ": " : ":");
        dumpTo(child, indent, depth + 1, os);
      }
      pad(indent, depth, os);
      os << '}';
      break;
    }
  }
}

}  // namespace

Value parse(std::string_view text, std::string* error) {
  if (error) error->clear();
  return Parser(text, error).parseRoot();
}

std::string Value::dump(int indent) const {
  std::ostringstream os;
  dumpTo(*this, indent, 0, os);
  os << '\n';
  return os.str();
}

}  // namespace media::json
