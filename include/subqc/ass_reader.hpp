#pragma once

#include <filesystem>
#include <string>
#include <vector>

#include "subqc/model.hpp"

namespace subqc {

std::string trim_copy(const std::string &value);
std::string lower_copy(std::string value);
double parse_ass_time(const std::string &value);
std::vector<std::string> split_ass_fields(const std::string &line, std::size_t max_fields);
std::vector<std::filesystem::path> collect_ass_files(const std::filesystem::path &input);
bool read_ass_file(const std::filesystem::path &path, AssFile &out);

}  // namespace subqc
