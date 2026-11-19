#pragma once

// Command-line option table for the terminal interface.
//
// Options are `--name value` pairs. Everything is validated here so the
// command handlers only see typed values; a parse failure carries a message
// suitable for the usage error path (exit code 2).

#include <cstdint>
#include <string>
#include <utility>
#include <vector>

namespace mfs {

struct OptionTable {
  std::vector<std::pair<std::string, std::string>> values;

  bool has(const std::string& name) const;
  bool get(const std::string& name, std::string& out) const;
  bool get_u64(const std::string& name, uint64_t& out) const;
  bool get_u32(const std::string& name, uint32_t& out) const;
  bool get_double(const std::string& name, double& out) const;
  std::vector<std::string> get_all(const std::string& name) const;
};

bool parse_options(const std::vector<std::string>& tokens, OptionTable& out,
                   std::string& error);

bool parse_u64(const std::string& text, uint64_t& out);
bool parse_double(const std::string& text, double& out);

// Small file helpers shared by the command handlers.
bool write_text_file(const std::string& path, const std::string& text);
bool read_text_file(const std::string& path, std::string& out);

}  // namespace mfs
