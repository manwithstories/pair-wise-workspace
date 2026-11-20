#include <catch2/catch_test_macros.hpp>

extern "C" {
#include <libavutil/pixfmt.h>
}

#include "mediacore/metadata.hpp"

using namespace mediacore;

namespace {
ColorMetadata color(int range, int primaries, int transfer, int matrix) {
    return ColorMetadata{range, primaries, transfer, matrix};
}
constexpr int R_MPEG = AVCOL_RANGE_MPEG;
constexpr int R_JPEG = AVCOL_RANGE_JPEG;
constexpr int P_709 = AVCOL_PRI_BT709;
constexpr int P_2020 = AVCOL_PRI_BT2020;
constexpr int T_709 = AVCOL_TRC_BT709;
constexpr int T_SRGB = AVCOL_TRC_IEC61966_2_1;
constexpr int M_709 = AVCOL_SPC_BT709;
}  // namespace

TEST_CASE("consistent color tags resolve without conflicts", "[color]") {
    ErrorLog errors;
    std::vector<ColorMetadata> segs{
        color(R_MPEG, P_709, T_709, M_709),
        color(R_MPEG, P_709, T_709, M_709)};
    auto report =
        checkColorMetadata("v0", segs, ColorPolicy::Strict, errors);
    REQUIRE_FALSE(report.incomplete);
    REQUIRE(report.conflicts.empty());
    REQUIRE(report.resolved.primaries == P_709);
    REQUIRE_FALSE(errors.hasFatal());
}

TEST_CASE("missing color fields are marked unknown, never invented",
          "[color]") {
    ErrorLog errors;
    std::vector<ColorMetadata> segs{color(0, P_709, 0, 0)};
    auto report =
        checkColorMetadata("v0", segs, ColorPolicy::Strict, errors);
    REQUIRE(report.incomplete);
    REQUIRE(report.resolved.range == 0);
    REQUIRE(errors.count(ErrorCode::ColorMetadataMissing) == 3);
    REQUIRE(colorRangeName(0) == std::string("unknown"));
}

TEST_CASE("strict policy turns color conflict into fatal error",
          "[color]") {
    ErrorLog errors;
    std::vector<ColorMetadata> segs{
        color(R_MPEG, P_709, T_709, M_709),
        color(R_JPEG, P_2020, T_SRGB, M_709)};
    auto report =
        checkColorMetadata("v0", segs, ColorPolicy::Strict, errors);
    REQUIRE(errors.count(ErrorCode::ColorConflict) >= 1);
    REQUIRE(errors.hasFatal());
    REQUIRE_FALSE(report.conflicts.empty());
}

TEST_CASE("prefer-srgb policy documents the resolution choice",
          "[color]") {
    ErrorLog errors;
    std::vector<ColorMetadata> segs{
        color(R_MPEG, P_709, T_709, M_709),
        color(R_JPEG, P_2020, T_SRGB, M_709)};
    auto report =
        checkColorMetadata("v0", segs, ColorPolicy::PreferSrgb, errors);
    REQUIRE_FALSE(errors.hasFatal());
    REQUIRE(report.resolved.primaries == P_709);  // sRGB default
    REQUIRE(report.resolved.transfer == T_SRGB);
    bool documented = false;
    for (const auto& c : report.conflicts)
        if (!c.decision.empty()) documented = true;
    REQUIRE(documented);
}

TEST_CASE("prefer-tag keeps the first tagged value on conflict",
          "[color]") {
    ErrorLog errors;
    std::vector<ColorMetadata> segs{
        color(R_MPEG, P_709, T_709, M_709),
        color(R_JPEG, P_2020, T_SRGB, M_709)};
    auto report =
        checkColorMetadata("v0", segs, ColorPolicy::PreferTag, errors);
    REQUIRE_FALSE(errors.hasFatal());
    REQUIRE(report.resolved.range == R_MPEG);
    REQUIRE(report.resolved.primaries == P_709);
}

TEST_CASE("policy parsing and rendering", "[color]") {
    REQUIRE(parseColorPolicy("prefer-srgb") == ColorPolicy::PreferSrgb);
    REQUIRE(parseColorPolicy("prefer-tag") == ColorPolicy::PreferTag);
    REQUIRE(parseColorPolicy("garbage") == ColorPolicy::Strict);
    REQUIRE(toString(ColorPolicy::Strict) == std::string("strict"));
}
