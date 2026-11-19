#include "cli_options.hpp"

#include <cerrno>
#include <cstdlib>
#include <fstream>
#include <iterator>

namespace mfs {

bool parse_u64(const std::string& text, uint64_t& out) {
  if (text.empty()) {
    return false;
  }
  errno = 0;
  char* end = nullptr;
  const unsigned long long value = std::strtoull(text.c_str(), &end, 10);
  if (errno != 0 || end == nullptr || *end != '\0') {
    return false;
  }
  out = value;
  return true;
}

bool parse_double(const std::string& text, double& out) {
  if (text.empty()) {
    return false;
  }
  errno = 0;
  char* end = nullptr;
  const double value = std::strtod(text.c_str(), &end);
  if (errno != 0 || end == nullptr || *end != '\0') {
    return false;
  }
  out = value;
  return true;
}

bool OptionTable::has(const std::string& name) const {
  for (const auto& entry : values) {
    if (entry.first == name) {
      return true;
    }
  }
  return false;
}

bool OptionTable::get(const std::string& name, std::string& out) const {
  for (const auto& entry : values) {
    if (entry.first == name) {
      out = entry.second;
      return true;
    }
  }
  return false;
}

bool OptionTable::get_u64(const std::string& name, uint64_t& out) const {
  std::string text;
  return get(name, text) && parse_u64(text, out);
}

bool OptionTable::get_u32(const std::string& name, uint32_t& out) const {
  uint64_t value = 0;
  if (!get_u64(name, value) || value > UINT32_MAX) {
    return false;
  }
  out = static_cast<uint32_t>(value);
  return true;
}

bool OptionTable::get_double(const std::string& name, double& out) const {
  std::string text;
  return get(name, text) && parse_double(text, out);
}

std::vector<std::string> OptionTable::get_all(const std::string& name) const {
  std::vector<std::string> result;
  for (const auto& entry : values) {
    if (entry.first == name) {
      result.push_back(entry.second);
    }
  }
  return result;
}

// Options are parsed as --name value pairs. Anything that is not `--` prefixed
// or lacks a value is a usage error.
bool parse_options(const std::vector<std::string>& tokens, OptionTable& out,
                   std::string& error) {
  for (size_t i = 0; i < tokens.size(); i += 2) {
    const std::string& token = tokens[i];
    if (token.rfind("--", 0) != 0) {
      error = "unexpected argument: " + token;
      return false;
    }
    if (i + 1 >= tokens.size()) {
      error = "missing value for " + token;
      return false;
    }
    out.values.emplace_back(token.substr(2), tokens[i + 1]);
  }
  return true;
}

bool write_text_file(const std::string& path, const std::string& text) {
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) {
    return false;
  }
  out.write(text.data(), static_cast<std::streamsize>(text.size()));
  return static_cast<bool>(out);
}

bool read_text_file(const std::string& path, std::string& out) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    return false;
  }
  out.assign(std::istreambuf_iterator<char>(in),
             std::istreambuf_iterator<char>());
  return true;
}

}  // namespace mfs
