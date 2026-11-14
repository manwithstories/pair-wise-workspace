#pragma once

#include <map>
#include <string>
#include <vector>

namespace h264 {

// Minimal JSON value model + parser, just enough to read baseline reports.
struct JsonValue {
  enum class Type { kNull, kBool, kNum, kStr, kArr, kObj };
  Type type = Type::kNull;
  bool boolean = false;
  double number = 0.0;
  std::string str;
  std::vector<JsonValue> arr;
  std::map<std::string, JsonValue> obj;

  const JsonValue* find(const std::string& key) const;
};

// Throws Error(kParse, ...) on malformed input.
JsonValue json_parse(const std::string& text);

}  // namespace h264
