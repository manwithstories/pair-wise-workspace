#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace media_restore {

// Minimal, strict JSON document model.  Archive manifests must round-trip
// exactly, so object keys keep their insertion order and the writer is
// canonical (2-space indent, one trailing newline).
class Json {
 public:
  enum class Type { null, boolean, integer, number, string, array, object };

  Json();
  explicit Json(bool value);
  explicit Json(int64_t value);
  explicit Json(double value);
  explicit Json(std::string value);
  // Without this overload a string literal would bind to Json(bool).
  explicit Json(const char* value);
  static Json array();
  static Json object();

  Type type() const { return type_; }
  bool is_null() const { return type_ == Type::null; }
  bool is_bool() const { return type_ == Type::boolean; }
  bool is_integer() const { return type_ == Type::integer; }
  bool is_number() const { return type_ == Type::integer || type_ == Type::number; }
  bool is_string() const { return type_ == Type::string; }
  bool is_array() const { return type_ == Type::array; }
  bool is_object() const { return type_ == Type::object; }

  bool as_bool() const { return bool_; }
  int64_t as_integer() const { return integer_; }
  double as_number() const;
  const std::string& as_string() const { return string_; }

  const std::vector<Json>& items() const { return items_; }
  const std::vector<std::pair<std::string, Json>>& members() const { return members_; }

  void push(Json value);
  Json& set(std::string key, Json value);
  const Json* find(std::string_view key) const;
  Json* find(std::string_view key);

  bool has(std::string_view key) const { return find(key) != nullptr; }

  // Parses `text`.  On failure returns a null document and fills `error` and
  // (when available) `error_offset` with a 0-based byte position.
  static Json parse(std::string_view text, std::string* error,
                    size_t* error_offset = nullptr);

  std::string dump() const;

  bool operator==(const Json& other) const;
  bool operator!=(const Json& other) const { return !(*this == other); }

 private:
  Type type_ = Type::null;
  bool bool_ = false;
  int64_t integer_ = 0;
  double number_ = 0.0;
  std::string string_;
  std::vector<Json> items_;
  std::vector<std::pair<std::string, Json>> members_;
};

}  // namespace media_restore
