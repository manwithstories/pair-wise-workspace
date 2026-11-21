#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace media::json {

// 轻量 JSON 值：只覆盖检查点与清单所需类型，键保持字典序便于稳定比较。
class Value {
 public:
  enum class Type { Null, Bool, Number, String, Array, Object };

  using Array = std::vector<Value>;
  using Object = std::map<std::string, Value>;

  Value() = default;
  static Value makeBool(bool b) { Value v; v.type_ = Type::Bool; v.bool_ = b; return v; }
  static Value makeNumber(int64_t n) { Value v; v.type_ = Type::Number; v.num_ = n; return v; }
  static Value makeString(std::string s) {
    Value v; v.type_ = Type::String; v.str_ = std::move(s); return v;
  }
  static Value makeArray() { Value v; v.type_ = Type::Array; return v; }
  static Value makeObject() { Value v; v.type_ = Type::Object; return v; }

  Type type() const { return type_; }
  bool isNull() const { return type_ == Type::Null; }
  bool asBool() const { return bool_; }
  int64_t asNumber() const { return num_; }
  const std::string& asString() const { return str_; }
  Array& asArray() { return arr_; }
  const Array& asArray() const { return arr_; }
  Object& asObject() { return obj_; }
  const Object& asObject() const { return obj_; }

  void push(Value v) { arr_.push_back(std::move(v)); type_ = Type::Array; }
  void set(const std::string& key, Value v) { obj_[key] = std::move(v); type_ = Type::Object; }
  const Value* find(const std::string& key) const {
    auto it = obj_.find(key);
    return it == obj_.end() ? nullptr : &it->second;
  }
  int64_t getNumber(const std::string& key, int64_t fallback) const {
    const Value* v = find(key);
    return v && v->type_ == Type::Number ? v->num_ : fallback;
  }
  std::string getString(const std::string& key, std::string fallback) const {
    const Value* v = find(key);
    return v && v->type_ == Type::String ? v->str_ : fallback;
  }

  std::string dump(int indent = 2) const;

 private:
  Type type_ = Type::Null;
  bool bool_ = false;
  int64_t num_ = 0;
  std::string str_;
  Array arr_;
  Object obj_;
};

// 解析失败返回 Null 类型值并设置 error（成功时 error 为空）。
Value parse(std::string_view text, std::string* error);

}  // namespace media::json
