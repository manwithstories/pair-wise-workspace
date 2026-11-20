#pragma once
// Minimal strict JSON value model used for manifests and index pages.
// No external dependency; sufficient for the mediaqc output vocabulary.
#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <vector>

namespace mediaqc::json {

class Value;
using Array = std::vector<Value>;
// Object key order is insertion order, preserved for stable manifests.
using Object = std::vector<std::pair<std::string, Value>>;

enum class Type { Null, Bool, Int, Double, String, Array, Object };

class Value {
public:
    Value() = default;
    Value(bool b) : type_(Type::Bool), bool_(b) {}
    Value(int v) : type_(Type::Int), int_(v) {}
    Value(std::int64_t v) : type_(Type::Int), int_(v) {}
    Value(double v) : type_(Type::Double), dbl_(v) {}
    Value(const char* s) : type_(Type::String), str_(s) {}
    Value(std::string s) : type_(Type::String), str_(std::move(s)) {}
    Value(Array a) : type_(Type::Array), arr_(std::move(a)) {}
    Value(Object o) : type_(Type::Object), obj_(std::move(o)) {}

    Type type() const { return type_; }
    bool is(Type t) const { return type_ == t; }
    bool as_bool() const { return bool_; }
    std::int64_t as_int() const { return int_; }
    double as_double() const { return type_ == Type::Int ? static_cast<double>(int_) : dbl_; }
    const std::string& as_string() const { return str_; }
    const Array& as_array() const { return arr_; }
    const Object& as_object() const { return obj_; }
    Object& as_object() { return obj_; }

    void set(std::string key, Value v);
    const Value* find(std::string_view key) const;

private:
    Type type_ = Type::Null;
    bool bool_ = false;
    std::int64_t int_ = 0;
    double dbl_ = 0.0;
    std::string str_;
    Array arr_;
    Object obj_;
};

std::string dump(const Value& v);

// Strict parse: throws std::runtime_error with a position on any syntax error.
Value parse(std::string_view text);

}  // namespace mediaqc::json
