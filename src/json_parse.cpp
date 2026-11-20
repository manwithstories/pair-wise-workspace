#include <cctype>
#include <cstdint>
#include <map>
#include <string>
#include <variant>
#include <vector>

#include "mediacore/json.hpp"

namespace mediacore::json {

// Compact strict JSON parser (input is produced by our own serializer, so
// no duplicate-key or tolerance handling is needed).
class Parser {
public:
    explicit Parser(std::string text) : text_(std::move(text)) {}

    bool parse(Value& out) {
        skipWs();
        if (!parseValue(out)) return false;
        skipWs();
        return pos_ == text_.size();
    }

private:
    void skipWs() {
        while (pos_ < text_.size() &&
               std::isspace(static_cast<unsigned char>(text_[pos_])))
            ++pos_;
    }

    bool parseValue(Value& out) {
        skipWs();
        if (pos_ >= text_.size()) return false;
        char c = text_[pos_];
        if (c == '{') return parseObject(out);
        if (c == '[') return parseArray(out);
        if (c == '"') {
            std::string s;
            if (!parseString(s)) return false;
            out = Value(std::move(s));
            return true;
        }
        if (c == 't' || c == 'f') return parseBool(out);
        if (c == 'n') return parseNull(out);
        return parseNumber(out);
    }

    bool parseObject(Value& out) {
        Value::Object obj;
        ++pos_;
        skipWs();
        if (pos_ < text_.size() && text_[pos_] == '}') { ++pos_; out = Value(std::move(obj)); return true; }
        while (pos_ < text_.size()) {
            skipWs();
            std::string key;
            if (!parseString(key)) return false;
            skipWs();
            if (pos_ >= text_.size() || text_[pos_] != ':') return false;
            ++pos_;
            Value v;
            if (!parseValue(v)) return false;
            obj.emplace(std::move(key), std::move(v));
            skipWs();
            if (pos_ >= text_.size()) return false;
            if (text_[pos_] == ',') { ++pos_; continue; }
            if (text_[pos_] == '}') { ++pos_; out = Value(std::move(obj)); return true; }
            return false;
        }
        return false;
    }

    bool parseArray(Value& out) {
        Value::Array arr;
        ++pos_;
        skipWs();
        if (pos_ < text_.size() && text_[pos_] == ']') { ++pos_; out = Value(std::move(arr)); return true; }
        while (pos_ < text_.size()) {
            Value v;
            if (!parseValue(v)) return false;
            arr.push_back(std::move(v));
            skipWs();
            if (pos_ >= text_.size()) return false;
            if (text_[pos_] == ',') { ++pos_; continue; }
            if (text_[pos_] == ']') { ++pos_; out = Value(std::move(arr)); return true; }
            return false;
        }
        return false;
    }

    bool parseString(std::string& out) {
        if (text_[pos_] != '"') return false;
        ++pos_;
        while (pos_ < text_.size() && text_[pos_] != '"') {
            char c = text_[pos_++];
            if (c == '\\') {
                if (pos_ >= text_.size()) return false;
                char e = text_[pos_++];
                switch (e) {
                    case 'n': out.push_back('\n'); break;
                    case 't': out.push_back('\t'); break;
                    case 'r': out.push_back('\r'); break;
                    case 'b': out.push_back('\b'); break;
                    case 'f': out.push_back('\f'); break;
                    case '"': out.push_back('"'); break;
                    case '\\': out.push_back('\\'); break;
                    case '/': out.push_back('/'); break;
                    case 'u': {
                        if (pos_ + 4 > text_.size()) return false;
                        int code = std::stoi(text_.substr(pos_, 4), nullptr, 16);
                        pos_ += 4;
                        if (code < 0x80) out.push_back(static_cast<char>(code));
                        else if (code < 0x800) {
                            out.push_back(static_cast<char>(0xC0 | (code >> 6)));
                            out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
                        } else {
                            out.push_back(static_cast<char>(0xE0 | (code >> 12)));
                            out.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
                            out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
                        }
                        break;
                    }
                    default: return false;
                }
            } else {
                out.push_back(c);
            }
        }
        if (pos_ >= text_.size()) return false;
        ++pos_;
        return true;
    }

    bool parseBool(Value& out) {
        if (text_.compare(pos_, 4, "true") == 0) { pos_ += 4; out = Value(true); return true; }
        if (text_.compare(pos_, 5, "false") == 0) { pos_ += 5; out = Value(false); return true; }
        return false;
    }

    bool parseNull(Value& out) {
        if (text_.compare(pos_, 4, "null") == 0) { pos_ += 4; out = Value(nullptr); return true; }
        return false;
    }

    bool parseNumber(Value& out) {
        std::size_t start = pos_;
        bool isFloat = false;
        if (text_[pos_] == '-') ++pos_;
        while (pos_ < text_.size()) {
            char c = text_[pos_];
            if (std::isdigit(static_cast<unsigned char>(c))) { ++pos_; }
            else if (c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
                isFloat = true;
                ++pos_;
            } else {
                break;
            }
        }
        if (pos_ == start) return false;
        const std::string token = text_.substr(start, pos_ - start);
        if (isFloat) { out = Value(std::stod(token)); return true; }
        out = Value(static_cast<int64_t>(std::stoll(token)));
        return true;
    }

    std::string text_;
    std::size_t pos_ = 0;
};

bool parse(const std::string& text, Value& out) {
    return Parser(text).parse(out);
}

}  // namespace mediacore::json
