#include "cli/Args.h"
#include "Errors.h"
#include <cstdlib>

namespace lg {

Args::Args(int argc, char** argv) {
  if (argc < 2)
    throw ArgsError("missing subcommand (probe|apply|batch|verify|demo)");
  cmd_ = argv[1];
  for (int i = 2; i < argc; ++i) {
    std::string tok = argv[i];
    if (tok.rfind("--", 0) != 0)
      throw ArgsError("unexpected argument: " + tok);
    std::string key = tok.substr(2);
    if (key.empty())
      throw ArgsError("empty option name");
    auto eq = key.find('=');
    if (eq != std::string::npos) {
      opts_[key.substr(0, eq)] = key.substr(eq + 1);
      continue;
    }
    if (i + 1 < argc && std::string(argv[i + 1]).rfind("--", 0) != 0)
      opts_[key] = argv[++i];
    else
      opts_[key] = "true"; // boolean flag
  }
}

bool Args::has(const std::string& key) const { return opts_.count(key) != 0; }

std::string Args::get(const std::string& key, const std::string& fallback) const {
  auto it = opts_.find(key);
  return it == opts_.end() ? fallback : it->second;
}

std::string Args::require(const std::string& key) const {
  auto it = opts_.find(key);
  if (it == opts_.end())
    throw ArgsError("missing required option --" + key);
  return it->second;
}

double Args::getDouble(const std::string& key, double fallback) const {
  auto it = opts_.find(key);
  if (it == opts_.end())
    return fallback;
  char* end = nullptr;
  double v = std::strtod(it->second.c_str(), &end);
  if (end == it->second.c_str() || *end != '\0')
    throw ArgsError("option --" + key + " expects a number, got '" + it->second + "'");
  return v;
}

int Args::getInt(const std::string& key, int fallback) const {
  return static_cast<int>(getDouble(key, fallback));
}

} // namespace lg
