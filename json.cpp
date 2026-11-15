#include "json.hpp"

#include <cctype>
#include <cstddef>
#include <limits>
#include <stdexcept>

namespace bake {

Json Json::null() { return Json{}; }

Json Json::boolean(bool value) {
    Json result;
    result.type = Type::boolean;
    result.bool_value = value;
    return result;
}

Json Json::integer(int64_t value) {
    Json result;
    result.type = Type::integer;
    result.int_value = value;
    return result;
}

Json Json::string(std::string value) {
    Json result;
    result.type = Type::string;
    result.string_value = std::move(value);
    return result;
}

Json Json::array() {
    Json result;
    result.type = Type::array;
    return result;
}

Json Json::object() {
    Json result;
    result.type = Type::object;
    return result;
}

Json& Json::set(std::string key, Json value) {
    if (type != Type::object) {
        throw std::runtime_error("JSON value is not an object");
    }
    object_value[std::move(key)] = std::move(value);
    return *this;
}

Json& Json::push(Json value) {
    if (type != Type::array) {
        throw std::runtime_error("JSON value is not an array");
    }
    array_value.push_back(std::move(value));
    return *this;
}

const Json* Json::find(const std::string& key) const {
    if (type != Type::object) {
        return nullptr;
    }
    const auto it = object_value.find(key);
    return it == object_value.end() ? nullptr : &it->second;
}

const Json& Json::at(const std::string& key) const {
    const Json* value = find(key);
    if (value == nullptr) {
        throw std::runtime_error("missing JSON key: " + key);
    }
    return *value;
}

Json& Json::at(const std::string& key) {
    const Json* value = find(key);
    if (value == nullptr) {
        throw std::runtime_error("missing JSON key: " + key);
    }
    return object_value.at(key);
}

namespace {

class Parser {
public:
    explicit Parser(const std::string& text) : text_(text) {}

    Json parse() {
        skipWhitespace();
        Json value = parseValue(0);
        skipWhitespace();
        if (position_ != text_.size()) {
            fail("trailing data");
        }
        return value;
    }

private:
    const std::string& text_;
    std::size_t position_ = 0;

    [[noreturn]] void fail(const std::string& message) const {
        throw std::runtime_error("JSON parse error at byte " +
                                 std::to_string(position_) + ": " + message);
    }

    void skipWhitespace() {
        while (position_ < text_.size()) {
            const unsigned char c = static_cast<unsigned char>(text_[position_]);
            if (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
                break;
            }
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

    void expect(char expected) {
        if (!consume(expected)) {
            fail(std::string("expected '") + expected + "'");
        }
    }

    Json parseValue(int depth) {
        if (depth > 128) {
            fail("nesting too deep");
        }
        skipWhitespace();
        if (position_ >= text_.size()) {
            fail("unexpected end");
        }
        const char c = text_[position_];
        if (c == '{') return parseObject(depth + 1);
        if (c == '[') return parseArray(depth + 1);
        if (c == '"') return Json::string(parseString());
        if (c == 't') return parseLiteral("true", Json::boolean(true));
        if (c == 'f') return parseLiteral("false", Json::boolean(false));
        if (c == 'n') return parseLiteral("null", Json::null());
        if (c == '-' || (c >= '0' && c <= '9')) return parseInteger();
        fail("unexpected token");
    }

    Json parseLiteral(const char* literal, Json value) {
        const std::size_t length = std::char_traits<char>::length(literal);
        if (text_.compare(position_, length, literal) != 0) {
            fail("invalid literal");
        }
        position_ += length;
        return value;
    }

    Json parseObject(int depth) {
        expect('{');
        Json result = Json::object();
        skipWhitespace();
        if (consume('}')) return result;
        while (true) {
            skipWhitespace();
            if (position_ >= text_.size() || text_[position_] != '"') {
                fail("object key must be a string");
            }
            const std::string key = parseString();
            skipWhitespace();
            expect(':');
            result.set(key, parseValue(depth));
            skipWhitespace();
            if (consume('}')) return result;
            expect(',');
        }
    }

    Json parseArray(int depth) {
        expect('[');
        Json result = Json::array();
        skipWhitespace();
        if (consume(']')) return result;
        while (true) {
            result.push(parseValue(depth));
            skipWhitespace();
            if (consume(']')) return result;
            expect(',');
        }
    }

    Json parseInteger() {
        const std::size_t start = position_;
        consume('-');
        if (position_ >= text_.size() || !std::isdigit(static_cast<unsigned char>(text_[position_]))) {
            fail("invalid integer");
        }
        if (text_[position_] == '0' && position_ + 1 < text_.size() &&
            std::isdigit(static_cast<unsigned char>(text_[position_ + 1]))) {
            fail("leading zero");
        }
        int64_t value = 0;
        while (position_ < text_.size() &&
               std::isdigit(static_cast<unsigned char>(text_[position_]))) {
            const int digit = text_[position_] - '0';
            if (value > (std::numeric_limits<int64_t>::max() - digit) / 10) {
                fail("integer overflow");
            }
            value = value * 10 + digit;
            ++position_;
        }
        if (position_ < text_.size() && text_[position_] == '-') {
            fail("misplaced minus");
        }
        if (start < position_ && text_[start] == '-') {
            value = -value;
        }
        return Json::integer(value);
    }

    static int hexValue(char c) {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        return -1;
    }

    static void appendUtf8(std::string& out, unsigned int cp) {
        if (cp <= 0x7f) {
            out.push_back(static_cast<char>(cp));
        } else if (cp <= 0x7ff) {
            out.push_back(static_cast<char>(0xc0 | (cp >> 6)));
            out.push_back(static_cast<char>(0x80 | (cp & 0x3f)));
        } else {
            out.push_back(static_cast<char>(0xe0 | (cp >> 12)));
            out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3f)));
            out.push_back(static_cast<char>(0x80 | (cp & 0x3f)));
        }
    }

    std::string parseString() {
        expect('"');
        std::string result;
        while (position_ < text_.size()) {
            const unsigned char c = static_cast<unsigned char>(text_[position_++]);
            if (c == '"') return result;
            if (c < 0x20) fail("control character in string");
            if (c != '\\') {
                result.push_back(static_cast<char>(c));
                continue;
            }
            if (position_ >= text_.size()) fail("unfinished escape");
            const char escaped = text_[position_++];
            switch (escaped) {
                case '"': result.push_back('"'); break;
                case '\\': result.push_back('\\'); break;
                case '/': result.push_back('/'); break;
                case 'b': result.push_back('\b'); break;
                case 'f': result.push_back('\f'); break;
                case 'n': result.push_back('\n'); break;
                case 'r': result.push_back('\r'); break;
                case 't': result.push_back('\t'); break;
                case 'u': {
                    if (position_ + 4 > text_.size()) fail("short unicode escape");
                    unsigned int cp = 0;
                    for (int i = 0; i < 4; ++i) {
                        const int digit = hexValue(text_[position_++]);
                        if (digit < 0) fail("invalid unicode escape");
                        cp = cp * 16 + static_cast<unsigned int>(digit);
                    }
                    if (cp >= 0xd800 && cp <= 0xdfff) fail("surrogate escape unsupported");
                    appendUtf8(result, cp);
                    break;
                }
                default: fail("unknown escape");
            }
        }
        fail("unterminated string");
    }
};

void dumpString(const std::string& value, std::string& out) {
    static const char* hex = "0123456789abcdef";
    out.push_back('"');
    for (const unsigned char c : value) {
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
                    out += "\\u00";
                    out.push_back(hex[c >> 4]);
                    out.push_back(hex[c & 0x0f]);
                } else {
                    out.push_back(static_cast<char>(c));
                }
        }
    }
    out.push_back('"');
}

void dumpValue(const Json& value, std::string& out) {
    switch (value.type) {
        case Json::Type::null_value: out += "null"; break;
        case Json::Type::boolean: out += value.bool_value ? "true" : "false"; break;
        case Json::Type::integer: out += std::to_string(value.int_value); break;
        case Json::Type::string: dumpString(value.string_value, out); break;
        case Json::Type::array:
            out.push_back('[');
            for (std::size_t i = 0; i < value.array_value.size(); ++i) {
                if (i != 0) out.push_back(',');
                dumpValue(value.array_value[i], out);
            }
            out.push_back(']');
            break;
        case Json::Type::object:
            out.push_back('{');
            {
                std::size_t i = 0;
                for (const auto& item : value.object_value) {
                    if (i++ != 0) out.push_back(',');
                    dumpString(item.first, out);
                    out.push_back(':');
                    dumpValue(item.second, out);
                }
            }
            out.push_back('}');
            break;
    }
}

}  // namespace

Json parseJson(const std::string& text) { return Parser(text).parse(); }

std::string dumpJson(const Json& value) {
    std::string result;
    result.reserve(256);
    dumpValue(value, result);
    return result;
}

}  // namespace bake
