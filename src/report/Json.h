#pragma once
#include <cstdint>
#include <string>
#include <utility>
#include <vector>

namespace lg {

// Minimal ordered JSON object writer (no third-party dependencies).
class JsonObject {
public:
  void setRaw(const std::string& key, const std::string& rawJson);
  void setString(const std::string& key, const std::string& value);
  void setInt(const std::string& key, long long value);
  void setNumber(const std::string& key, double value); // 6 decimals
  void setBool(const std::string& key, bool value);
  void setObject(const std::string& key, const JsonObject& obj);
  void setArrayU64(const std::string& key,
                   const std::vector<std::uint64_t>& values);
  std::string str() const;

private:
  std::vector<std::pair<std::string, std::string>> entries_;
};

void writeJsonFile(const JsonObject& obj, const std::string& path);

} // namespace lg
