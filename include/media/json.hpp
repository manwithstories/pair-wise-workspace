#pragma once

#include <cstdint>
#include <map>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace mediaqc {

// A tiny, dependency-free JSON value used for supplier metadata, the
// generated manifests and the sidecar index.  Objects keep their keys
// sorted so repeated runs serialize byte-identically.
class Json {
 public:
  enum class Type { kNull, kBool, kInt, kDouble, kString, kArray, kObject };

  Json() = default;
  static Json make_null();
  static Json make_bool(bool value);
  static Json make_int(std::int64_t value);
  static Json make_double(double value);
  static Json make_string(std::string value);
  static Json make_array(std::vector<Json> values = {});
  static Json make_object(std::map<std::string, Json, std::less<>> values = {});

  [[nodiscard]] Type type() const { return type_; }
  [[nodiscard]] bool is_null() const { return type_ == Type::kNull; }
  [[nodiscard]] bool is_bool() const { return type_ == Type::kBool; }
  [[nodiscard]] bool is_int() const { return type_ == Type::kInt; }
  [[nodiscard]] bool is_double() const { return type_ == Type::kDouble; }
  [[nodiscard]] bool is_number() const { return is_int() || is_double(); }
  [[nodiscard]] bool is_string() const { return type_ == Type::kString; }
  [[nodiscard]] bool is_array() const { return type_ == Type::kArray; }
  [[nodiscard]] bool is_object() const { return type_ == Type::kObject; }

  [[nodiscard]] bool as_bool(bool fallback = false) const;
  [[nodiscard]] std::int64_t as_int(std::int64_t fallback = 0) const;
  [[nodiscard]] double as_double(double fallback = 0.0) const;
  [[nodiscard]] const std::string& as_string() const;

  [[nodiscard]] const std::vector<Json>& items() const { return array_; }
  std::vector<Json>& items() { return array_; }
  [[nodiscard]] const std::map<std::string, Json, std::less<>>& members() const {
    return object_;
  }

  // Object lookup; returns nullptr when the key is absent.
  [[nodiscard]] const Json* find(std::string_view key) const;
  Json& operator[](std::string key);
  void set(std::string key, Json value);
  void push(Json value);

  [[nodiscard]] std::size_t size() const;

 private:
  Type type_ = Type::kNull;
  bool bool_ = false;
  std::int64_t int_ = 0;
  double double_ = 0.0;
  std::string string_;
  std::vector<Json> array_;
  std::map<std::string, Json, std::less<>> object_;
};

struct JsonParseResult {
  bool ok = false;
  Json value;
  std::string error;      // human readable, empty when ok
  std::size_t error_at = 0;  // byte offset of the failure
};

// Parses one JSON document.  Trailing non-whitespace is an error so a
// truncated or doubled line is rejected rather than silently accepted.
[[nodiscard]] JsonParseResult parse_json(std::string_view text);

// Serializes compactly; `pretty` adds two-space indentation.  Keys are
// emitted in sorted order because the object map is ordered.
[[nodiscard]] std::string dump_json(const Json& value, bool pretty = false);

}  // namespace mediaqc
