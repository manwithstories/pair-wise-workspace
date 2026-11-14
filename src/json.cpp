#include "json.h"

#include "error.h"

#include <cmath>
#include <cstdlib>
#include <iomanip>
#include <sstream>

namespace mp4 {

Json::Json() = default;

Json Json::object() { Json value; value.type_ = Type::Object; return value; }
Json Json::array() { Json value; value.type_ = Type::Array; return value; }
Json Json::string(std::string value) { Json v; v.type_ = Type::String; v.string_ = std::move(value); return v; }
Json Json::boolean(bool value) { Json v; v.type_ = Type::Bool; v.bool_ = value; return v; }
Json Json::integer(int64_t value) { Json v; v.type_ = Type::Int; v.int_ = value; return v; }
Json Json::number(double value) { Json v; v.type_ = Type::Number; v.number_ = value; return v; }

void Json::set(std::string key, Json value) {
    if (type_ != Type::Object) { type_ = Type::Object; }
    for (auto& member : object_) {
        if (member.first == key) { member.second = std::move(value); return; }
    }
    object_.emplace_back(std::move(key), std::move(value));
}

Json& Json::push(Json value) {
    if (type_ != Type::Array) { type_ = Type::Array; }
    array_.push_back(std::move(value));
    return array_.back();
}

const Json* Json::find(const std::string& key) const {
    if (type_ != Type::Object) return nullptr;
    for (const auto& member : object_) {
        if (member.first == key) return &member.second;
    }
    return nullptr;
}

const Json& Json::at(size_t index) const {
    if (type_ != Type::Array || index >= array_.size()) {
        fail(ExitCode::FormatError, "JSON array index out of bounds");
    }
    return array_[index];
}

size_t Json::size() const noexcept {
    if (type_ == Type::Array) return array_.size();
    if (type_ == Type::Object) return object_.size();
    return 0;
}

bool Json::as_bool(bool fallback) const { return type_ == Type::Bool ? bool_ : fallback; }
int64_t Json::as_i64(int64_t fallback) const {
    if (type_ == Type::Int) return int_;
    if (type_ == Type::Number) return static_cast<int64_t>(number_);
    return fallback;
}
uint64_t Json::as_u64(uint64_t fallback) const {
    if (type_ != Type::Int || int_ < 0) return fallback;
    return static_cast<uint64_t>(int_);
}
double Json::as_number(double fallback) const {
    if (type_ == Type::Number) return number_;
    if (type_ == Type::Int) return static_cast<double>(int_);
    return fallback;
}
const std::string& Json::as_string() const {
    if (type_ != Type::String) fail(ExitCode::FormatError, "JSON value is not a string");
    return string_;
}

namespace {

void escape_json(std::string& out, const std::string& value) {
    out.push_back('"');
    for (unsigned char ch : value) {
        switch (ch) {
            case '"': out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\b': out += "\\b"; break;
            case '\f': out += "\\f"; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (ch < 0x20) {
                    std::ostringstream oss;
                    oss << "\\u" << std::hex << std::setw(4) << std::setfill('0') << int(ch);
                    out += oss.str();
                } else {
                    out.push_back(static_cast<char>(ch));
                }
        }
    }
    out.push_back('"');
}

class Parser {
public:
    explicit Parser(const std::string& text) : text_(text) {}
    Json parse() {
        skip_space();
        Json value = parse_value();
        skip_space();
        if (pos_ != text_.size()) error("trailing characters");
        return value;
    }

private:
    [[noreturn]] void error(const std::string& what) const {
        fail(ExitCode::FormatError, "invalid JSON at byte " + std::to_string(pos_) + ": " + what);
    }
    void skip_space() {
        while (pos_ < text_.size() && (text_[pos_] == ' ' || text_[pos_] == '\n' ||
               text_[pos_] == '\r' || text_[pos_] == '\t')) ++pos_;
    }
    char take() {
        if (pos_ >= text_.size()) error("unexpected end of input");
        return text_[pos_++];
    }
    void expect(char expected) { if (take() != expected) error(std::string("expected '") + expected + "'"); }
    Json parse_value() {
        skip_space();
        if (pos_ >= text_.size()) error("expected value");
        const char ch = text_[pos_];
        if (ch == '{') return parse_object();
        if (ch == '[') return parse_array();
        if (ch == '"') return Json::string(parse_string());
        if (ch == 't') { literal("true"); return Json::boolean(true); }
        if (ch == 'f') { literal("false"); return Json::boolean(false); }
        if (ch == 'n') { literal("null"); return Json(); }
        return parse_number();
    }
    void literal(const std::string& word) {
        if (text_.compare(pos_, word.size(), word) != 0) error("invalid literal");
        pos_ += word.size();
    }
    std::string parse_string() {
        expect('"');
        std::string out;
        while (true) {
            const char ch = take();
            if (ch == '"') break;
            if (static_cast<unsigned char>(ch) < 0x20) error("control character in string");
            if (ch != '\\') { out.push_back(ch); continue; }
            const char esc = take();
            switch (esc) {
                case '"': out.push_back('"'); break;
                case '\\': out.push_back('\\'); break;
                case '/': out.push_back('/'); break;
                case 'b': out.push_back('\b'); break;
                case 'f': out.push_back('\f'); break;
                case 'n': out.push_back('\n'); break;
                case 'r': out.push_back('\r'); break;
                case 't': out.push_back('\t'); break;
                case 'u': parse_unicode(out); break;
                default: error("invalid escape");
            }
        }
        return out;
    }
    void parse_unicode(std::string& out) {
        if (pos_ + 4 > text_.size()) error("truncated unicode escape");
        unsigned code = 0;
        for (int i = 0; i < 4; ++i) {
            const char ch = take();
            code <<= 4;
            if (ch >= '0' && ch <= '9') code += static_cast<unsigned>(ch - '0');
            else if (ch >= 'a' && ch <= 'f') code += static_cast<unsigned>(ch - 'a' + 10);
            else if (ch >= 'A' && ch <= 'F') code += static_cast<unsigned>(ch - 'A' + 10);
            else error("invalid unicode escape");
        }
        if (code <= 0x7f) out.push_back(static_cast<char>(code));
        else if (code <= 0x7ff) {
            out.push_back(static_cast<char>(0xc0 | (code >> 6)));
            out.push_back(static_cast<char>(0x80 | (code & 0x3f)));
        } else {
            out.push_back(static_cast<char>(0xe0 | (code >> 12)));
            out.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3f)));
            out.push_back(static_cast<char>(0x80 | (code & 0x3f)));
        }
    }
    Json parse_number() {
        const size_t start = pos_;
        if (text_[pos_] == '-') ++pos_;
        while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
        bool floating = false;
        if (pos_ < text_.size() && text_[pos_] == '.') {
            floating = true; ++pos_;
            while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
        }
        if (pos_ < text_.size() && (text_[pos_] == 'e' || text_[pos_] == 'E')) {
            floating = true; ++pos_;
            if (pos_ < text_.size() && (text_[pos_] == '+' || text_[pos_] == '-')) ++pos_;
            while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
        }
        if (start == pos_) error("invalid number");
        const std::string token = text_.substr(start, pos_ - start);
        if (!floating) {
            char* end = nullptr;
            const long long value = std::strtoll(token.c_str(), &end, 10);
            if (end != nullptr && *end == '\0') return Json::integer(value);
        }
        return Json::number(std::strtod(token.c_str(), nullptr));
    }
    Json parse_array() {
        expect('[');
        Json result = Json::array();
        skip_space();
        if (pos_ < text_.size() && text_[pos_] == ']') { ++pos_; return result; }
        while (true) {
            result.push(parse_value());
            skip_space();
            const char ch = take();
            if (ch == ']') break;
            if (ch != ',') error("expected ',' or ']'");
        }
        return result;
    }
    Json parse_object() {
        expect('{');
        Json result = Json::object();
        skip_space();
        if (pos_ < text_.size() && text_[pos_] == '}') { ++pos_; return result; }
        while (true) {
            skip_space();
            std::string key = parse_string();
            skip_space();
            expect(':');
            result.set(std::move(key), parse_value());
            skip_space();
            const char ch = take();
            if (ch == '}') break;
            if (ch != ',') error("expected ',' or '}'");
        }
        return result;
    }
    const std::string& text_;
    size_t pos_ = 0;
};

}  // namespace

void Json::dump_to(std::string& out, int indent, int depth) const {
    const std::string pad(static_cast<size_t>(indent * depth), ' ');
    const std::string child_pad(static_cast<size_t>(indent * (depth + 1)), ' ');
    switch (type_) {
        case Type::Null: out += "null"; break;
        case Type::Bool: out += bool_ ? "true" : "false"; break;
        case Type::Int: out += std::to_string(int_); break;
        case Type::Number: {
            std::ostringstream oss;
            oss << std::setprecision(17) << number_;
            out += oss.str();
            break;
        }
        case Type::String: escape_json(out, string_); break;
        case Type::Array:
            if (array_.empty()) { out += "[]"; break; }
            out += "[\n";
            for (size_t i = 0; i < array_.size(); ++i) {
                out += child_pad;
                array_[i].dump_to(out, indent, depth + 1);
                if (i + 1 < array_.size()) out += ',';
                out += '\n';
            }
            out += pad + "]";
            break;
        case Type::Object:
            if (object_.empty()) { out += "{}"; break; }
            out += "{\n";
            for (size_t i = 0; i < object_.size(); ++i) {
                out += child_pad;
                escape_json(out, object_[i].first);
                out += ": ";
                object_[i].second.dump_to(out, indent, depth + 1);
                if (i + 1 < object_.size()) out += ',';
                out += '\n';
            }
            out += pad + "}";
            break;
    }
}

std::string Json::dump(int indent) const {
    std::string out;
    dump_to(out, indent, 0);
    out.push_back('\n');
    return out;
}

Json Json::parse(const std::string& text) { return Parser(text).parse(); }

}  // namespace mp4
