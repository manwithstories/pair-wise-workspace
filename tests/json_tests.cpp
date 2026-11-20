#include "media/json.hpp"

#include <catch2/catch_test_macros.hpp>

#include <string>

using namespace mediaqc;

TEST_CASE("json values round-trip through dump and parse", "[json]") {
  Json root = Json::make_object();
  root.set("int", Json::make_int(-42));
  root.set("double", Json::make_double(3.5));
  root.set("bool", Json::make_bool(true));
  root.set("null", Json::make_null());
  root.set("text", Json::make_string("line\n\"quote\"\ttab é"));
  Json array = Json::make_array();
  array.push(Json::make_int(1));
  array.push(Json::make_string("two"));
  root.set("list", std::move(array));

  const std::string text = dump_json(root);
  JsonParseResult parsed = parse_json(text);
  REQUIRE(parsed.ok);
  CHECK(parsed.value.find("int")->as_int() == -42);
  CHECK(parsed.value.find("double")->as_double() == 3.5);
  CHECK(parsed.value.find("bool")->as_bool());
  CHECK(parsed.value.find("null")->is_null());
  CHECK(parsed.value.find("text")->as_string() == "line\n\"quote\"\ttab é");
  REQUIRE(parsed.value.find("list")->items().size() == 2);
  CHECK(parsed.value.find("list")->items()[1].as_string() == "two");
}

TEST_CASE("object keys serialize in sorted order for reproducibility", "[json]") {
  Json object = Json::make_object();
  object.set("zebra", Json::make_int(1));
  object.set("alpha", Json::make_int(2));
  object.set("mike", Json::make_int(3));
  CHECK(dump_json(object) == R"({"alpha":2,"mike":3,"zebra":1})");
}

TEST_CASE("malformed json reports the failure position", "[json]") {
  auto truncated = parse_json(R"({"a":1)");
  CHECK_FALSE(truncated.ok);
  CHECK_FALSE(truncated.error.empty());

  auto trailing = parse_json(R"({"a":1} extra)");
  CHECK_FALSE(trailing.ok);

  auto bad = parse_json("[1,2,]");
  CHECK_FALSE(bad.ok);
}

TEST_CASE("unicode escapes decode to UTF-8", "[json]") {
  auto parsed = parse_json(R"({"s":"é中"})");
  REQUIRE(parsed.ok);
  CHECK(parsed.value.find("s")->as_string() == "é中");
}

TEST_CASE("doubles remain distinguishable from integers", "[json]") {
  auto parsed = parse_json(R"({"i":5,"d":5.0,"e":1e3})");
  REQUIRE(parsed.ok);
  CHECK(parsed.value.find("i")->is_int());
  CHECK(parsed.value.find("d")->is_double());
  CHECK(parsed.value.find("e")->as_double() == 1000.0);
}
