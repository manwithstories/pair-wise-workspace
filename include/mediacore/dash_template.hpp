#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace mediacore {

// DASH $RepresentationID$ / $Number$ / $Time$ template expansion.
std::string substituteTemplate(std::string tmpl, const std::string& id,
                               int64_t number, int64_t time);

// Split a DASH codecs attribute ("avc1.64001f, mp4a.40.2").
std::vector<std::string> codecList(const std::string& s);

}  // namespace mediacore
