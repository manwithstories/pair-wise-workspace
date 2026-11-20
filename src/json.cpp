#include "mediacore/json.hpp"

#include <cmath>
#include <iomanip>
#include <sstream>

namespace mediacore::json {

std::string escape(const std::string& s) {
    std::string out;
    out.reserve(s.size() + 2);
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
                    out += static_cast<char>(c);
                }
        }
    }
    return out;
}

namespace {
void dumpTo(const Value& v, int indent, int depth, std::string& out);

void pad(int indent, int depth, std::string& out) {
    if (indent <= 0) return;
    out.append(static_cast<std::size_t>(indent * depth), ' ');
}

void dumpTo(const Value& v, int indent, int depth, std::string& out) {
    visitValue(v, [&](const auto& x) {
            using T = std::decay_t<decltype(x)>;
            if constexpr (std::is_same_v<T, std::monostate>) {
                out += "null";
            } else if constexpr (std::is_same_v<T, bool>) {
                out += x ? "true" : "false";
            } else if constexpr (std::is_same_v<T, int64_t>) {
                out += std::to_string(x);
            } else if constexpr (std::is_same_v<T, double>) {
                std::ostringstream ss;
                ss << std::setprecision(17) << x;
                out += ss.str();
            } else if constexpr (std::is_same_v<T, std::string>) {
                out += '"';
                out += escape(x);
                out += '"';
            } else if constexpr (std::is_same_v<T, Value::Array>) {
                if (x.empty()) { out += "[]"; return; }
                out += '[';
                for (std::size_t i = 0; i < x.size(); ++i) {
                    if (i) out += ',';
                    if (indent > 0) { out += '\n'; pad(indent, depth + 1, out); }
                    dumpTo(x[i], indent, depth + 1, out);
                }
                if (indent > 0) { out += '\n'; pad(indent, depth, out); }
                out += ']';
            } else if constexpr (std::is_same_v<T, Value::Object>) {
                if (x.empty()) { out += "{}"; return; }
                out += '{';
                std::size_t i = 0;
                for (const auto& [k, val] : x) {
                    if (i++) out += ',';
                    if (indent > 0) { out += '\n'; pad(indent, depth + 1, out); }
                    out += '"';
                    out += escape(k);
                    out += indent > 0 ? "\": " : "\":";
                    dumpTo(val, indent, depth + 1, out);
                }
                if (indent > 0) { out += '\n'; pad(indent, depth, out); }
                out += '}';
            }
        });
}
}  // namespace

std::string dump(const Value& v, int indent) {
    std::string out;
    dumpTo(v, indent, 0, out);
    out += '\n';
    return out;
}

std::string dumpInline(const Value& v) {
    std::string out;
    dumpTo(v, 0, 0, out);
    return out;
}

}  // namespace mediacore::json
