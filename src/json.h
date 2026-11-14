#pragma once

#include <cstdint>
#include <string>
#include <utility>
#include <vector>

namespace mp4 {

class Json {
public:
    enum class Type { Null, Bool, Int, Number, String, Array, Object };

    Json();
    static Json object();
    static Json array();
    static Json string(std::string value);
    static Json boolean(bool value);
    static Json integer(int64_t value);
    static Json number(double value);

    Type type() const noexcept { return type_; }
    bool is_null() const noexcept { return type_ == Type::Null; }
    bool is_object() const noexcept { return type_ == Type::Object; }
    bool is_array() const noexcept { return type_ == Type::Array; }
    bool is_integer() const noexcept { return type_ == Type::Int; }
    bool is_string() const noexcept { return type_ == Type::String; }

    void set(std::string key, Json value);
    Json& push(Json value);
    const Json* find(const std::string& key) const;
    const Json& at(size_t index) const;
    size_t size() const noexcept;

    bool as_bool(bool fallback = false) const;
    int64_t as_i64(int64_t fallback = 0) const;
    uint64_t as_u64(uint64_t fallback = 0) const;
    double as_number(double fallback = 0.0) const;
    const std::string& as_string() const;
    const std::vector<Json>& items() const { return array_; }
    const std::vector<std::pair<std::string, Json>>& members() const { return object_; }

    std::string dump(int indent = 2) const;
    static Json parse(const std::string& text);

private:
    void dump_to(std::string& out, int indent, int depth) const;

    Type type_ = Type::Null;
    bool bool_ = false;
    int64_t int_ = 0;
    double number_ = 0.0;
    std::string string_;
    std::vector<Json> array_;
    std::vector<std::pair<std::string, Json>> object_;
};

}  // namespace mp4
