#include "json.h"

#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <limits>

namespace fmp4 {

namespace {

[[noreturn]] void badJson(const std::string& source, size_t position,
                          const std::string& message) {
    throw parseError(source + ": invalid JSON (" + message + ")", position);
}

void appendUtf8(std::string& out, uint32_t code) {
    if (code <= 0x7F) {
        out.push_back(static_cast<char>(code));
    } else if (code <= 0x7FF) {
        out.push_back(static_cast<char>(0xC0 | (code >> 6)));
        out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
    } else {
        out.push_back(static_cast<char>(0xE0 | (code >> 12)));
        out.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
    }
}

}  // namespace

class JsonParser {
public:
    JsonParser(const std::string& text, const std::string& source)
        : text_(text), source_(source) {}

    JsonValue run() {
        JsonValue value = parseValue();
        whitespace();
        if (position_ != text_.size()) {
            badJson(source_, position_, "trailing data");
        }
        return value;
    }

private:
    void whitespace() {
        while (position_ < text_.size()) {
            const char c = text_[position_];
            if (c != ' ' && c != '\t' && c != '\n' && c != '\r') break;
            ++position_;
        }
    }

    bool consume(char expected) {
        if (position_ < text_.size() && text_[position_] == expected) {
            ++position_;
            return true;
        }
        return false;
    }

    void expect(char c) {
        if (!consume(c)) badJson(source_, position_, std::string("expected '") + c + "'");
    }

    JsonValue parseValue() {
        whitespace();
        if (position_ >= text_.size()) badJson(source_, position_, "unexpected end");
        const char c = text_[position_];
        if (c == '{') return parseObject();
        if (c == '[') return parseArray();
        if (c == '"') return parseStringValue();
        if (c == 't') return literal("true", JsonValue::Type::Boolean, true);
        if (c == 'f') return literal("false", JsonValue::Type::Boolean, false);
        if (c == 'n') return literal("null", JsonValue::Type::Null, false);
        if (c == '-' || (c >= '0' && c <= '9')) return parseNumber();
        badJson(source_, position_, "unexpected character");
    }

    JsonValue literal(const char* word, JsonValue::Type type, bool boolean) {
        const std::string token(word);
        if (text_.compare(position_, token.size(), token) != 0) {
            badJson(source_, position_, "invalid literal");
        }
        position_ += token.size();
        JsonValue result;
        result.type_ = type;
        result.boolean_ = boolean;
        return result;
    }

    JsonValue parseObject() {
        JsonValue result;
        result.type_ = JsonValue::Type::Object;
        expect('{');
        whitespace();
        if (consume('}')) return result;
        for (;;) {
            whitespace();
            if (position_ >= text_.size() || text_[position_] != '"') {
                badJson(source_, position_, "object key must be a string");
            }
            std::string key = parseString();
            whitespace();
            expect(':');
            JsonValue value = parseValue();
            if (!result.object_.emplace(key, std::move(value)).second) {
                badJson(source_, position_, "duplicate object key");
            }
            whitespace();
            if (consume('}')) break;
            expect(',');
        }
        return result;
    }

    JsonValue parseArray() {
        JsonValue result;
        result.type_ = JsonValue::Type::Array;
        expect('[');
        whitespace();
        if (consume(']')) return result;
        for (;;) {
            result.array_.push_back(parseValue());
            whitespace();
            if (consume(']')) break;
            expect(',');
        }
        return result;
    }

    std::string parseString() {
        expect('"');
        std::string result;
        while (position_ < text_.size()) {
            const unsigned char c = static_cast<unsigned char>(text_[position_++]);
            if (c == '"') return result;
            if (c < 0x20) badJson(source_, position_ - 1, "control character in string");
            if (c != '\\') {
                result.push_back(static_cast<char>(c));
                continue;
            }
            if (position_ >= text_.size()) badJson(source_, position_, "truncated escape");
            const char escape = text_[position_++];
            switch (escape) {
                case '"': result.push_back('"'); break;
                case '\\': result.push_back('\\'); break;
                case '/': result.push_back('/'); break;
                case 'b': result.push_back('\b'); break;
                case 'f': result.push_back('\f'); break;
                case 'n': result.push_back('\n'); break;
                case 'r': result.push_back('\r'); break;
                case 't': result.push_back('\t'); break;
                case 'u': {
                    if (position_ + 4 > text_.size()) badJson(source_, position_, "short unicode escape");
                    uint32_t code = 0;
                    for (int i = 0; i < 4; ++i) {
                        const char h = text_[position_++];
                        code <<= 4;
                        if (h >= '0' && h <= '9') code |= static_cast<uint32_t>(h - '0');
                        else if (h >= 'a' && h <= 'f') code |= static_cast<uint32_t>(h - 'a' + 10);
                        else if (h >= 'A' && h <= 'F') code |= static_cast<uint32_t>(h - 'A' + 10);
                        else badJson(source_, position_ - 1, "invalid unicode escape");
                    }
                    appendUtf8(result, code);
                    break;
                }
                default: badJson(source_, position_ - 1, "invalid escape");
            }
        }
        badJson(source_, position_, "unterminated string");
    }

    JsonValue parseStringValue() {
        JsonValue result;
        result.type_ = JsonValue::Type::String;
        result.string_ = parseString();
        return result;
    }

    JsonValue parseNumber() {
        const size_t begin = position_;
        consume('-');
        if (consume('0')) {
            if (position_ < text_.size() && text_[position_] >= '0' && text_[position_] <= '9') {
                badJson(source_, position_, "leading zero in number");
            }
        } else {
            if (position_ >= text_.size() || text_[position_] < '1' || text_[position_] > '9') {
                badJson(source_, begin, "invalid number");
            }
            while (position_ < text_.size() && text_[position_] >= '0' && text_[position_] <= '9') {
                ++position_;
            }
        }
        bool floating = false;
        if (consume('.')) {
            floating = true;
            if (position_ >= text_.size() || text_[position_] < '0' || text_[position_] > '9') {
                badJson(source_, position_, "invalid fraction");
            }
            while (position_ < text_.size() && text_[position_] >= '0' && text_[position_] <= '9') {
                ++position_;
            }
        }
        if (position_ < text_.size() && (text_[position_] == 'e' || text_[position_] == 'E')) {
            floating = true;
            ++position_;
            if (position_ < text_.size() && (text_[position_] == '+' || text_[position_] == '-')) {
                ++position_;
            }
            if (position_ >= text_.size() || text_[position_] < '0' || text_[position_] > '9') {
                badJson(source_, position_, "invalid exponent");
            }
            while (position_ < text_.size() && text_[position_] >= '0' && text_[position_] <= '9') {
                ++position_;
            }
        }
        const std::string token = text_.substr(begin, position_ - begin);
        JsonValue result;
        if (!floating) {
            errno = 0;
            char* end = nullptr;
            const long long value = std::strtoll(token.c_str(), &end, 10);
            if (errno == 0 && end == token.c_str() + token.size()) {
                result.type_ = JsonValue::Type::Integer;
                result.integer_ = static_cast<int64_t>(value);
                result.number_ = static_cast<double>(value);
                return result;
            }
        }
        char* end = nullptr;
        const double value = std::strtod(token.c_str(), &end);
        if (end != token.c_str() + token.size() || !std::isfinite(value)) {
            badJson(source_, begin, "invalid number value");
        }
        result.type_ = JsonValue::Type::Number;
        result.number_ = value;
        return result;
    }

    const std::string& text_;
    const std::string& source_;
    size_t position_ = 0;
};

bool JsonValue::boolean() const {
    if (type_ != Type::Boolean) throw Error(ExitCode::ParseFailed, "JSON value is not boolean");
    return boolean_;
}

int64_t JsonValue::integer() const {
    if (type_ == Type::Integer) return integer_;
    if (type_ == Type::Number && std::floor(number_) == number_) {
        return static_cast<int64_t>(number_);
    }
    throw Error(ExitCode::ParseFailed, "JSON value is not an integer");
}

double JsonValue::number() const {
    if (type_ == Type::Integer) return static_cast<double>(integer_);
    if (type_ == Type::Number) return number_;
    throw Error(ExitCode::ParseFailed, "JSON value is not a number");
}

const std::string& JsonValue::string() const {
    if (type_ != Type::String) throw Error(ExitCode::ParseFailed, "JSON value is not a string");
    return string_;
}

const std::vector<JsonValue>& JsonValue::array() const {
    if (type_ != Type::Array) throw Error(ExitCode::ParseFailed, "JSON value is not an array");
    return array_;
}

const std::map<std::string, JsonValue>& JsonValue::object() const {
    if (type_ != Type::Object) throw Error(ExitCode::ParseFailed, "JSON value is not an object");
    return object_;
}

bool JsonValue::contains(const std::string& key) const {
    return type_ == Type::Object && object_.find(key) != object_.end();
}

const JsonValue& JsonValue::at(const std::string& key) const {
    const JsonValue* value = find(key);
    if (value == nullptr) {
        throw Error(ExitCode::ParseFailed, "JSON object is missing key: " + key);
    }
    return *value;
}

const JsonValue* JsonValue::find(const std::string& key) const {
    if (type_ != Type::Object) return nullptr;
    const auto it = object_.find(key);
    return it == object_.end() ? nullptr : &it->second;
}

JsonValue JsonValue::parse(const std::string& text, const std::string& source_name) {
    return JsonParser(text, source_name).run();
}

}  // namespace fmp4

