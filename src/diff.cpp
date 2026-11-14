#include "diff.h"

#include <cstdio>
#include <map>

#include "json.h"

namespace h264 {

namespace {

std::string scalar_to_string(const JsonValue& v) {
  char buf[32];
  switch (v.type) {
    case JsonValue::Type::kNull: return "null";
    case JsonValue::Type::kBool: return v.boolean ? "true" : "false";
    case JsonValue::Type::kNum:
      snprintf(buf, sizeof(buf), "%.10g", v.number);
      return buf;
    case JsonValue::Type::kStr: return "\"" + v.str + "\"";
    default: return "";
  }
}

bool is_scalar(const JsonValue& v) {
  return v.type == JsonValue::Type::kNull || v.type == JsonValue::Type::kBool ||
         v.type == JsonValue::Type::kNum || v.type == JsonValue::Type::kStr;
}

void flatten(const JsonValue& v, const std::string& path,
             std::map<std::string, std::string>& out) {
  if (is_scalar(v)) {
    out[path] = scalar_to_string(v);
    return;
  }
  if (v.type == JsonValue::Type::kArr) {
    if (v.arr.empty()) {
      out[path + "[]"] = "";
    }
    for (size_t i = 0; i < v.arr.size(); ++i) {
      flatten(v.arr[i], path + "[" + std::to_string(i) + "]", out);
    }
    return;
  }
  if (v.obj.empty()) {
    out[path + "{}"] = "";
  }
  for (const auto& kv : v.obj) {
    flatten(kv.second, path.empty() ? kv.first : path + "." + kv.first, out);
  }
}

bool excluded(const std::string& path) {
  return path == "file" || path.compare(0, 6, "stats.") == 0 || path == "stats";
}

}  // namespace

std::vector<DiffEntry> diff_reports(const std::string& left_json,
                                    const std::string& right_json) {
  JsonValue left = json_parse(left_json);
  JsonValue right = json_parse(right_json);
  std::map<std::string, std::string> lf, rf;
  flatten(left, "", lf);
  flatten(right, "", rf);
  std::vector<DiffEntry> diffs;
  for (const auto& kv : lf) {
    if (excluded(kv.first)) {
      continue;
    }
    auto it = rf.find(kv.first);
    if (it == rf.end()) {
      diffs.push_back({kv.first, kv.second, "<missing>"});
    } else if (it->second != kv.second) {
      diffs.push_back({kv.first, kv.second, it->second});
    }
  }
  for (const auto& kv : rf) {
    if (excluded(kv.first)) {
      continue;
    }
    if (lf.find(kv.first) == lf.end()) {
      diffs.push_back({kv.first, "<missing>", kv.second});
    }
  }
  return diffs;
}

}  // namespace h264
