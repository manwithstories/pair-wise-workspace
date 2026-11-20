#include "mediacore/dash_template.hpp"

namespace mediacore {

std::string substituteTemplate(std::string tmpl, const std::string& id,
                               int64_t number, int64_t time) {
    auto replaceAll = [&](const std::string& token,
                          const std::string& value) {
        std::size_t p;
        while ((p = tmpl.find(token)) != std::string::npos)
            tmpl.replace(p, token.size(), value);
    };
    replaceAll("$RepresentationID$", id);
    replaceAll("$Number$", std::to_string(number));
    replaceAll("$Time$", std::to_string(time));
    return tmpl;
}

std::vector<std::string> codecList(const std::string& s) {
    std::vector<std::string> out;
    std::string token;
    for (char c : s) {
        if (c == ',' || c == ' ') {
            if (!token.empty()) out.push_back(token);
            token.clear();
        } else {
            token.push_back(c);
        }
    }
    if (!token.empty()) out.push_back(token);
    return out;
}

}  // namespace mediacore
