#pragma once
#include <map>
#include <string>

namespace lg {

// Parses: <command> [--key value] [--key=value] [--flag]
class Args {
public:
  Args(int argc, char** argv);
  const std::string& command() const { return cmd_; }
  bool has(const std::string& key) const;
  std::string get(const std::string& key, const std::string& fallback = "") const;
  std::string require(const std::string& key) const;
  double getDouble(const std::string& key, double fallback) const;
  int getInt(const std::string& key, int fallback) const;

private:
  std::string cmd_;
  std::map<std::string, std::string> opts_;
};

} // namespace lg
