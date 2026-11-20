#include <catch2/catch_test_macros.hpp>

#include "mediacore/error.hpp"
#include "mediacore/json.hpp"
#include "mediacore/pathutil.hpp"
#include "mediacore/time.hpp"

using namespace mediacore;

TEST_CASE("rational rescale stays exact or refuses", "[time]") {
    Rational ms{1, 1000};
    Rational us{1, 1'000'000};
    REQUIRE(scaleExact(250, ms, us).value() == 250'000);
    REQUIRE_FALSE(scaleExact(1, us, ms).has_value());  // non-divisible
    REQUIRE(scaleExact(1000, us, ms).value() == 1);

    Rational pal{1001, 30000};  // 29.97 fps frame ticks
    Rational sec{1, 1};
    REQUIRE(scaleExact(30000, pal, sec).value() == 1001);
    REQUIRE_FALSE(scaleExact(1, pal, sec).has_value());
}

TEST_CASE("cross-base comparison never uses floating point", "[time]") {
    Rational a{1, 30}, b{1, 29};
    REQUIRE(compareTick(1, a, 1, b) < 0);
    REQUIRE(compareTick(30, Rational{1, 30}, 29, Rational{1, 29}) == 0);
    REQUIRE(compareTick(1, Rational{1, 30}, 1, Rational{1, 29}) < 0);
    REQUIRE(compareTick(2, Rational{1, 30}, 1, Rational{1, 29}) > 0);
}

TEST_CASE("decimal parsing is exact", "[time]") {
    auto r = parseDecimal("1.001").value();
    REQUIRE(Rational::reduced(1001, 1000).num == r.num);
    REQUIRE(r.den == 1000);
    REQUIRE_FALSE(parseDecimal("1.x").has_value());
    REQUIRE_FALSE(parseDecimal("-3").has_value());
    REQUIRE(parseDecimal("48000")->den == 1);
}

TEST_CASE("range overlap and exact range scaling", "[time]") {
    TimeRange a{0, 100, Rational{1, 1000}};
    TimeRange b{50, 150, Rational{1, 1000}};
    TimeRange c{100, 200, Rational{1, 1000}};
    REQUIRE(overlaps(a, b));
    REQUIRE_FALSE(overlaps(a, c));  // half-open
    auto s = scaleRangeExact(a, Rational{1, 1'000'000}).value();
    REQUIRE(s.start == 0);
    REQUIRE(s.end == 100'000);
}

TEST_CASE("error log tracks fatal vs recoverable", "[error]") {
    ErrorLog log;
    log.add({ErrorCode::SegmentCorrupt, "v0", 2, "x.m4s",
             {0, 1, {1, 1}}, true});
    log.add({ErrorCode::DependencyCycle, "g", -1, "",
             {0, 0, {0, 1}}, false});
    REQUIRE(log.size() == 2);
    REQUIRE(log.count(ErrorCode::SegmentCorrupt) == 1);
    REQUIRE(log.hasFatal());
    REQUIRE(isRecoverable(ErrorCode::SegmentMissing));
    REQUIRE_FALSE(isRecoverable(ErrorCode::ManifestTruncated));
    REQUIRE(log.forTrack("v0").size() == 1);
}

TEST_CASE("path containment blocks traversal and schemes", "[path]") {
    auto root = std::filesystem::temp_directory_path() / "mcaudit_root";
    std::filesystem::create_directories(root / "sub");
    REQUIRE(constrainPath(root, root / "sub" / "seg.m4s").has_value());
    REQUIRE_FALSE(
        constrainPath(root, root / ".." / "etc" / "passwd").has_value());
    auto md = root / "manifest";
    std::filesystem::create_directories(md);
    REQUIRE(resolveManifestUri(md, "../sub/seg.m4s", root).has_value());
    REQUIRE_FALSE(
        resolveManifestUri(md, "https://host/seg.m4s", root).has_value());
    REQUIRE_FALSE(
        resolveManifestUri(md, "../../../escape.m4s", root).has_value());
}

TEST_CASE("json output is ordered and stable", "[json]") {
    json::ObjectBuilder b;
    b.add("z", json::Value(1)).add("a", json::Value("x"));
    const std::string text = json::dumpInline(b.take());
    REQUIRE(text == R"({"a":"x","z":1})");
    REQUIRE(json::dumpInline(json::Value::array()) == "[]");
    REQUIRE(json::escape("a\nb\"") == "a\\nb\\\"");
}
