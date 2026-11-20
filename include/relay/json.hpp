#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <string_view>
#include <vector>

namespace relay::json {

class Value;
using Array = std::vector<Value>;
/// std::map keeps keys sorted, which makes round-trips byte-stable.
using Object = std::map<std::string, Value>;

enum class Type { kNull, kBool, kNumber, kString, kArray, kObject };

class Value {
 public:
  Value() = default;
  Value(std::nullptr_t) {}
  Value(bool value) : type_(Type::kBool), bool_(value) {}
  Value(int value)
      : type_(Type::kNumber),
        number_(static_cast<double>(value)) {}
  Value(std::int64_t value)
      : type_(Type::kNumber),
        number_(static_cast<double>(value)) {}
  Value(std::uint64_t value)
      : type_(Type::kNumber),
        number_(static_cast<double>(value)) {}
  Value(double value) : type_(Type::kNumber), number_(value) {}
  Value(std::string value) : type_(Type::kString), string_(std::move(value)) {}
  Value(const char* value) : type_(Type::kString), string_(value) {}
  Value(Array value) : type_(Type::kArray), array_(std::move(value)) {}
  Value(Object value) : type_(Type::kObject), object_(std::move(value)) {}

  [[nodiscard]] Type type() const { return type_; }
  [[nodiscard]] bool is_null() const { return type_ == Type::kNull; }
  [[nodiscard]] bool is_object() const { return type_ == Type::kObject; }
  [[nodiscard]] bool is_array() const { return type_ == Type::kArray; }
  [[nodiscard]] bool is_string() const { return type_ == Type::kString; }
  [[nodiscard]] bool is_number() const { return type_ == Type::kNumber; }

  [[nodiscard]] bool as_bool() const { return bool_; }
  [[nodiscard]] double as_number() const { return number_; }
  [[nodiscard]] std::int64_t as_int() const {
    return static_cast<std::int64_t>(number_);
  }
  [[nodiscard]] const std::string& as_string() const { return string_; }
  [[nodiscard]] const Array& as_array() const { return array_; }
  [[nodiscard]] Array& as_array() { return array_; }
  [[nodiscard]] const Object& as_object() const { return object_; }
  [[nodiscard]] Object& as_object() { return object_; }

  /// Non-throwing lookup; nullptr when absent or not an object.
  [[nodiscard]] const Value* find(std::string_view key) const;
  Value& operator[](const std::string& key);
  void push(Value value);
  [[nodiscard]] bool contains(std::string_view key) const;

  [[nodiscard]] std::string dump(int indent = 2) const;

 private:
  void dump_into(std::string& out, int indent, int depth) const;

  Type type_ = Type::kNull;
  bool bool_ = false;
  double number_ = 0.0;
  std::string string_;
  Array array_;
  Object object_;
};

struct ParseError {
  std::size_t offset = 0;
  std::string message;
};

/// Accepts strict JSON (comments and trailing commas rejected) so a damaged
/// manifest is detected rather than silently reparsed.
[[nodiscard]] bool parse(std::string_view text, Value& out, ParseError& error);

}  // namespace relay::json
