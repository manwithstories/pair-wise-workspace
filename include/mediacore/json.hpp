#pragma once

#include <cstdint>
#include <map>
#include <sstream>
#include <string>
#include <variant>
#include <vector>

namespace mediacore::json {

// Minimal deterministic JSON value. Object keys are kept in an ordered map
// so repeated runs over identical input emit byte-identical summaries.
class Value {
public:
    using Array = std::vector<Value>;
    using Object = std::map<std::string, Value>;

    Value() = default;
    Value(std::nullptr_t) {}
    Value(bool b) : data_(b) {}
    Value(int64_t i) : data_(i) {}
    Value(int i) : data_(static_cast<int64_t>(i)) {}
    Value(double d) : data_(d) {}
    Value(const char* s) : data_(std::string(s)) {}
    Value(std::string s) : data_(std::move(s)) {}
    Value(Array a) : data_(std::move(a)) {}
    Value(Object o) : data_(std::move(o)) {}

    bool isObject() const { return std::holds_alternative<Object>(data_); }
    bool isArray() const { return std::holds_alternative<Array>(data_); }
    const Object& asObject() const { return std::get<Object>(data_); }
    Object& asObject() { return std::get<Object>(data_); }
    const Array& asArray() const { return std::get<Array>(data_); }

    static Value object() { return Value(Object{}); }
    static Value array() { return Value(Array{}); }

private:
    std::variant<std::monostate, bool, int64_t, double, std::string,
                 Array, Object>
        data_;

    template <class Visitor>
    friend auto visitValue(const Value& v, Visitor&& vis) {
        return std::visit(std::forward<Visitor>(vis), v.data_);
    }

    friend std::string dump(const Value&, int);
    friend std::string dumpInline(const Value&);
};

std::string escape(const std::string& s);
std::string dump(const Value& v, int indent = 2);
std::string dumpInline(const Value& v);

// Convenience builder for ordered objects.
class ObjectBuilder {
public:
    ObjectBuilder& add(const std::string& key, Value v) {
        obj_.emplace(key, std::move(v));
        return *this;
    }
    Value take() { return Value(std::move(obj_)); }

private:
    Value::Object obj_;
};

}  // namespace mediacore::json
