// 媒体时钟：回绕展开与异常时间戳边界路径覆盖。
#include "media_clock.hpp"
#include <catch2/catch_test_macros.hpp>
#include <catch2/catch_approx.hpp>

using namespace ma;
using Catch::Approx;

TEST_CASE("clock: 单调正常序列不产生异常", "[clock]") {
    StreamClock clk(1000);
    for (int64_t t : {100, 140, 180, 220}) {
        REQUIRE(clk.feed(t) == t);
    }
    REQUIRE(clk.wrapCount() == 0);
    for (const auto& e : clk.events()) {
        REQUIRE(e.kind == TsKind::Normal);
    }
}

TEST_CASE("clock: 33 位回绕被展开且原始值保留", "[clock]") {
    const int64_t M = 1LL << 33;
    StreamClock clk(M);
    REQUIRE(clk.feed(M - 200) == M - 200);
    const int64_t expanded = clk.feed(300);
    REQUIRE(expanded == M + 300);
    REQUIRE(clk.wrapCount() == 1);
    const auto& last = clk.events().back();
    REQUIRE(last.kind == TsKind::Wrap);
    REQUIRE(last.raw == 300);          // 原始回绕值保留
    REQUIRE(last.expanded == M + 300); // 展开值单调
}

TEST_CASE("clock: 重复与非回绕倒退被区分", "[clock]") {
    StreamClock clk(1LL << 33);
    clk.feed(1000);
    clk.feed(1000);
    REQUIRE(clk.events().back().kind == TsKind::Repeat);
    clk.reset();
    clk.feed(1000);
    clk.feed(900); // 倒退 100，远小于模数一半 → 普通倒退
    REQUIRE(clk.events().back().kind == TsKind::Backward);
    REQUIRE(clk.wrapCount() == 0);
}

TEST_CASE("clock: 间隔远超期望判为缺失", "[clock]") {
    StreamClock clk(1000000); // 模数很大，排除回绕歧义
    clk.feed(0);
    clk.feed(40);
    clk.feed(80);
    clk.feed(80 + 40 * 5); // 5 倍期望间隔
    REQUIRE(clk.events().back().kind == TsKind::Missing);
}

TEST_CASE("clock: 多次回绕持续单调", "[clock]") {
    const int64_t M = 1000;
    StreamClock clk(M);
    REQUIRE(clk.feed(900) == 900);
    REQUIRE(clk.feed(100) == M + 100);       // 回绕 1
    REQUIRE(clk.feed(200) == M + 200);
    REQUIRE(clk.feed(100) == 2 * M + 100);   // 回绕 2
    REQUIRE(clk.feed(250) == 2 * M + 250);
    REQUIRE(clk.feed(50) == 3 * M + 50);     // 回绕 3
    REQUIRE(clk.wrapCount() == 3);
}

TEST_CASE("clock: reset 清空状态", "[clock]") {
    StreamClock clk(1000);
    clk.feed(950);
    clk.feed(50);
    clk.reset();
    REQUIRE(clk.wrapCount() == 0);
    REQUIRE(clk.events().empty());
    REQUIRE(clk.feed(10) == 10);
}

TEST_CASE("clock: 时间基转换与百分位", "[clock]") {
    // 90kHz 时间基下 900 ticks = 10ms
    REQUIRE(StreamClock::toMs(900, 1, 90000) == Approx(10.0));
    std::vector<double> v{1.0, 2.0, 3.0, 4.0};
    REQUIRE(percentileMs(v, 95) == Approx(3.85));
    REQUIRE(percentileMs({}, 50) == 0.0);
}
