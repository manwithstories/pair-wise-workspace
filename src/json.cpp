#include "media/json.hpp"

#include <cctype>
#include <cmath>
#include <cstdio>
#include <sstream>
#include <stdexcept>

namespace mediaqc::json {

void Value::set(std::string key, Value v) {
    for (auto& kv : obj_) {
        if (kv.first == key) { kv.second = std::move(v); return; }
    }
    obj_.emplace_back(std::move(key), std::move(v));
}

const Value* Value::find(std::string_view key) const {
    for (const auto& kv : obj_)
        if (kv.first == key) return &kv.second;
    return nullptr;
}

namespace {

void escape_string(std::string& out, std::string_view s) {
    out.push_back('"');
    for (unsigned char c : s) {
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
                    std::snprintf(buf, sizeof(buf), "\\u%04x", c);
                    out += buf;
                } else {
                    out.push_back(static_cast<char>(c));
                }
        }
    }
    out.push_back('"');
}

void write(std::string& out, const Value& v, int indent, int depth) {
    auto pad = [&](int d) { if (indent >= 0) out.append(static_cast<size_t>(d) * indent, ' '); };
    switch (v.type()) {
        case Type::Null: out += "null"; break;
        case Type::Bool: out += v.as_bool() ? "true" : "false"; break;
        case Type::Int: out += std::to_string(v.as_int()); break;
        case Type::Double: {
            char buf[32];
            std::snprintf(buf, sizeof(buf), "%.6g", v.as_double());
            out += buf;
            break;
        }
        case Type::String: escape_string(out, v.as_string()); break;
        case Type::Array: {
            const auto& a = v.as_array();
            if (a.empty()) { out += "[]"; break; }
            out.push_back('[');
            for (size_t i = 0; i < a.size(); ++i) {
                if (i) out.push_back(',');
                if (indent >= 0) { out.push_back('\n'); pad(depth + 1); }
                write(out, a[i], indent, depth + 1);
            }
            if (indent >= 0) { out.push_back('\n'); pad(depth); }
            out.push_back(']');
            break;
        }
        case Type::Object: {
            const auto& o = v.as_object();
            if (o.empty()) { out += "{}"; break; }
            out.push_back('{');
            for (size_t i = 0; i < o.size(); ++i) {
                if (i) out.push_back(',');
                if (indent >= 0) { out.push_back('\n'); pad(depth + 1); }
                escape_string(out, o[i].first);
                out += indent >= 0 ? ": " : ":";
                write(out, o[i].second, indent, depth + 1);
            }
            if (indent >= 0) { out.push_back('\n'); pad(depth); }
            out.push_back('}');
            break;
        }
    }
}

class Parser {
public:
    explicit Parser(std::string_view t) : t_(t) {}

    Value parse() {
        skip_ws();
        Value v = parse_value();
        skip_ws();
        if (pos_ != t_.size()) fail("trailing characters");
        return v;
    }

private:
    std::string_view t_;
    size_t pos_ = 0;

    [[noreturn]] void fail(const std::string& msg) const {
        throw std::runtime_error("json parse error at byte " + std::to_string(pos_) + ": " + msg);
    }
    char peek() const { return pos_ < t_.size() ? t_[pos_] : '\0'; }
    void skip_ws() { while (pos_ < t_.size() && (t_[pos_] == ' ' || t_[pos_] == '\t' ||
                                                  t_[pos_] == '\n' || t_[pos_] == '\r')) ++pos_; }
    bool consume(char c) { if (peek() == c) { ++pos_; return true; } return false; }
    void expect(char c) { if (!consume(c)) fail(std::string("expected '") + c + "'"); }

    Value parse_value() {
        skip_ws();
        switch (peek()) {
            case '{': return parse_object();
            case '[': return parse_array();
            case '"': return parse_string();
            case 't': case 'f': return parse_bool();
            case 'n': return parse_null();
            default: return parse_number();
        }
    }

    Value parse_object() {
        expect('{');
        Object o;
        skip_ws();
        if (consume('}')) return Value(std::move(o));
        while (true) {
            skip_ws();
            if (peek() != '"') fail("expected string key");
            std::string key = parse_string_raw();
            skip_ws();
            expect(':');
            o.emplace_back(std::move(key), parse_value());
            skip_ws();
            if (consume(',')) continue;
            expect('}');
            return Value(std::move(o));
        }
    }

    Value parse_array() {
        expect('[');
        Array a;
        skip_ws();
        if (consume(']')) return Value(std::move(a));
        while (true) {
            a.push_back(parse_value());
            skip_ws();
            if (consume(',')) continue;
            expect(']');
            return Value(std::move(a));
        }
    }

    std::string parse_string_raw() {
        expect('"');
        std::string s;
        while (pos_ < t_.size()) {
            char c = t_[pos_++];
            if (c == '"') return s;
            if (c == '\\') {
                if (pos_ >= t_.size()) fail("bad escape");
                char e = t_[pos_++];
                switch (e) {
                    case '"': s.push_back('"'); break;
                    case '\\': s.push_back('\\'); break;
                    case '/': s.push_back('/'); break;
                    case 'b': s.push_back('\b'); break;
                    case 'f': s.push_back('\f'); break;
                    case 'n': s.push_back('\n'); break;
                    case 'r': s.push_back('\r'); break;
                    case 't': s.push_back('\t'); break;
                    case 'u': {
                        if (pos_ + 4 > t_.size()) fail("bad unicode escape");
                        unsigned code = 0;
                        for (int i = 0; i < 4; ++i) {
                            char h = t_[pos_++];
                            code <<= 4;
                            if (h >= '0' && h <= '9') code |= static_cast<unsigned>(h - '0');
                            else if (h >= 'a' && h <= 'f') code |= static_cast<unsigned>(h - 'a' + 10);
                            else if (h >= 'A' && h <= 'F') code |= static_cast<unsigned>(h - 'A' + 10);
                            else fail("bad hex digit");
                        }
                        if (code < 0x80) s.push_back(static_cast<char>(code));
                        else if (code < 0x800) {
                            s.push_back(static_cast<char>(0xC0 | (code >> 6)));
                            s.push_back(static_cast<char>(0x80 | (code & 0x3F)));
                        } else {
                            s.push_back(static_cast<char>(0xE0 | (code >> 12)));
                            s.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
                            s.push_back(static_cast<char>(0x80 | (code & 0x3F)));
                        }
                        break;
                    }
                    default: fail("bad escape character");
                }
            } else if (static_cast<unsigned char>(c) < 0x20) {
                fail("unescaped control character");
            } else {
                s.push_back(c);
            }
        }
        fail("unterminated string");
    }

    Value parse_string() { return Value(parse_string_raw()); }

    Value parse_bool() {
        if (t_.compare(pos_, 4, "true") == 0) { pos_ += 4; return Value(true); }
        if (t_.compare(pos_, 5, "false") == 0) { pos_ += 5; return Value(false); }
        fail("bad literal");
    }

    Value parse_null() {
        if (t_.compare(pos_, 4, "null") == 0) { pos_ += 4; return Value(); }
        fail("bad literal");
    }

    Value parse_number() {
        size_t start = pos_;
        if (peek() == '-') ++pos_;
        bool any_digit = false;
        while (std::isdigit(static_cast<unsigned char>(peek()))) { ++pos_; any_digit = true; }
        bool is_double = false;
        if (peek() == '.') {
            is_double = true;
            ++pos_;
            while (std::isdigit(static_cast<unsigned char>(peek()))) ++pos_;
        }
        if (peek() == 'e' || peek() == 'E') {
            is_double = true;
            ++pos_;
            if (peek() == '+' || peek() == '-') ++pos_;
            while (std::isdigit(static_cast<unsigned char>(peek()))) ++pos_;
        }
        if (!any_digit) fail("bad number");
        std::string num(t_.substr(start, pos_ - start));
        return is_double ? Value(std::stod(num)) : Value(static_cast<std::int64_t>(std::stoll(num)));
    }
};

}  // namespace

std::string dump(const Value& v) {
    std::string out;
    write(out, v, 2, 0);
    out.push_back('\n');
    return out;
}

Value parse(std::string_view text) { return Parser(text).parse(); }

}  // namespace mediaqc::json
