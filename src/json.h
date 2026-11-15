#pragma once

#include "error.h"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace fmp4 {

class JsonValue {
public:
    enum class Type { Null, Boolean, Integer, Number, String, Array, Object };

    Type type() const { return type_; }
    bool boolean() const;
    int64_t integer() const;
    double number() const;
    const std::string& string() const;
    const std::vector<JsonValue>& array() const;
    const std::map<std::string, JsonValue>& object() const;
    bool contains(const std::string& key) const;
    const JsonValue& at(const std::string& key) const;
    const JsonValue* find(const std::string& key) const;

    static JsonValue parse(const std::string& text, const std::string& source_name);

private:
    friend class JsonParser;
    Type type_ = Type::Null;
    bool boolean_ = false;
    int64_t integer_ = 0;
    double number_ = 0.0;
    std::string string_;
    std::vector<JsonValue> array_;
    std::map<std::string, JsonValue> object_;
};

}  // namespace fmp4

