#include "report/Json.h"
#include "Errors.h"
#include <cmath>
#include <cstdio>
#include <fstream>

namespace lg {

void JsonObject::setRaw(const std::string& key, const std::string& rawJson) {
  entries_.emplace_back(key, rawJson);
}

void JsonObject::setString(const std::string& key, const std::string& value) {
  std::string esc = "\"";
  for (char c : value) {
    if (c == '"' || c == '\\')
      esc += '\\';
    esc += c;
  }
  esc += '"';
  setRaw(key, esc);
}

void JsonObject::setInt(const std::string& key, long long value) {
  setRaw(key, std::to_string(value));
}

void JsonObject::setNumber(const std::string& key, double value) {
  if (!std::isfinite(value)) {
    setString(key, "inf");
    return;
  }
  char buf[64];
  std::snprintf(buf, sizeof(buf), "%.6f", value);
  setRaw(key, buf);
}

void JsonObject::setBool(const std::string& key, bool value) {
  setRaw(key, value ? "true" : "false");
}

void JsonObject::setObject(const std::string& key, const JsonObject& obj) {
  setRaw(key, obj.str());
}

void JsonObject::setArrayU64(const std::string& key,
                             const std::vector<std::uint64_t>& values) {
  std::string s = "[";
  for (std::size_t i = 0; i < values.size(); ++i) {
    if (i)
      s += ",";
    s += std::to_string(values[i]);
  }
  s += "]";
  setRaw(key, s);
}

std::string JsonObject::str() const {
  std::string s = "{";
  for (std::size_t i = 0; i < entries_.size(); ++i) {
    if (i)
      s += ",";
    s += "\"" + entries_[i].first + "\":" + entries_[i].second;
  }
  s += "}";
  return s;
}

void writeJsonFile(const JsonObject& obj, const std::string& path) {
  std::ofstream out(path);
  if (!out)
    throw IoError("cannot write report: " + path);
  out << obj.str() << "\n";
  if (!out)
    throw IoError("failed writing report: " + path);
}

} // namespace lg
