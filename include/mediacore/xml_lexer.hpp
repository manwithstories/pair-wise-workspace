#pragma once

#include <cctype>
#include <cstdint>
#include <map>
#include <string>

namespace mediacore {

struct XmlTag {
    std::string name;
    std::map<std::string, std::string> attrs;
    bool closing = false;   // </foo>
    bool selfClose = false; // <foo/>
};

// Streaming XML tokenizer covering the element/attribute subset used by
// MPD files. Comments and processing instructions are skipped.
class XmlLexer {
public:
    explicit XmlLexer(std::string text) : text_(std::move(text)) {}

    bool next(XmlTag& out) {
        while (pos_ < text_.size()) {
            std::size_t open = text_.find('<', pos_);
            if (open == std::string::npos) return false;
            if (text_.compare(open, 4, "<!--") == 0) {
                auto end = text_.find("-->", open + 4);
                pos_ = end == std::string::npos ? text_.size() : end + 3;
                continue;
            }
            if (open + 1 < text_.size() && text_[open + 1] == '?') {
                auto end = text_.find("?>", open + 2);
                pos_ = end == std::string::npos ? text_.size() : end + 2;
                continue;
            }
            auto close = text_.find('>', open);
            if (close == std::string::npos) {
                truncated_ = true;
                return false;
            }
            std::string body = text_.substr(open + 1, close - open - 1);
            pos_ = close + 1;
            if (!parseTag(body, out)) continue;
            return true;
        }
        return false;
    }

    bool truncated() const { return truncated_; }

private:
    bool parseTag(const std::string& body, XmlTag& out) const {
        if (body.empty()) return false;
        std::size_t i = 0;
        out = XmlTag{};
        if (body[0] == '/') {
            out.closing = true;
            out.name = body.substr(1);
            return true;
        }
        while (i < body.size() &&
               !std::isspace(static_cast<unsigned char>(body[i])) &&
               body[i] != '/')
            out.name.push_back(body[i++]);
        while (i < body.size()) {
            while (i < body.size() &&
                   std::isspace(static_cast<unsigned char>(body[i])))
                ++i;
            if (i >= body.size() || body[i] == '/') break;
            std::string key;
            while (i < body.size() && body[i] != '=' &&
                   !std::isspace(static_cast<unsigned char>(body[i])))
                key.push_back(body[i++]);
            while (i < body.size() && body[i] != '=') {
                if (body[i] != ' ' && body[i] != '\t') break;
                ++i;
            }
            if (i >= body.size() || body[i] != '=') break;
            ++i;
            while (i < body.size() &&
                   std::isspace(static_cast<unsigned char>(body[i])))
                ++i;
            std::string val;
            if (i < body.size() && body[i] == '"') {
                ++i;
                while (i < body.size() && body[i] != '"')
                    val.push_back(body[i++]);
                ++i;
            }
            if (!key.empty()) out.attrs.emplace(key, val);
        }
        if (!body.empty() && body.back() == '/') out.selfClose = true;
        return true;
    }

    std::string text_;
    std::size_t pos_ = 0;
    bool truncated_ = false;
};

}  // namespace mediacore
