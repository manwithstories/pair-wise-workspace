#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <utility>
#include <vector>

namespace bake {

struct Json {
    enum class Type { null_value, boolean, integer, string, array, object };

    Type type = Type::null_value;
    bool bool_value = false;
    int64_t int_value = 0;
    std::string string_value;
    std::vector<Json> array_value;
    std::map<std::string, Json> object_value;

    static Json null();
    static Json boolean(bool value);
    static Json integer(int64_t value);
    static Json string(std::string value);
    static Json array();
    static Json object();

    Json& set(std::string key, Json value);
    Json& push(Json value);
    const Json* find(const std::string& key) const;
    const Json& at(const std::string& key) const;
    Json& at(const std::string& key);

    bool isObject() const { return type == Type::object; }
    bool isArray() const { return type == Type::array; }
    bool isInteger() const { return type == Type::integer; }
    bool isString() const { return type == Type::string; }
};

Json parseJson(const std::string& text);
std::string dumpJson(const Json& value);

}  // namespace bake
