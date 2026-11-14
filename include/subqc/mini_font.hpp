#pragma once

#include <filesystem>
#include <string>

namespace subqc {

bool write_minimal_font(const std::filesystem::path &path, const std::string &family,
                        std::string &error);

}  // namespace subqc
