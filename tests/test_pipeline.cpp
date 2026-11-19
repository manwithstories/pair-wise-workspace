// 损坏定位与流式管线状态/边界路径单测。
#include "damage_locator.hpp"
#include <catch2/catch_test_macros.hpp>
#include <catch2/catch_approx.hpp>

using namespace ma;
using Catch::Approx;

TEST_CASE("damage: 连续失败合并为区间且后续成功计恢复", "[damage]") {
    DamageLocator loc;
    // 10 个预期包：第 3、4 包失败，其余成功
    for (int64_t i = 0; i < 10; ++i) {
        const bool ok = (i != 3 && i != 4);
        loc.onPacket(ok, i, i * 100, i * 40.0);
    }
    loc.finish(false);
    REQUIRE(loc.ranges().size() == 1);
    const auto& r = loc.ranges().front();
    REQUIRE(r.startPacket == 3);
    REQUIRE(r.endPacket == 4);
    REQUIRE(r.startFilePos == 300);
    REQUIRE(r.errorPackets == 2);
    REQUIRE(r.recoveredPackets == 5); // 包 5..9（窗口收敛后仍累计在区间）
    const auto rec = loc.recovery();
    REQUIRE(rec.totalPackets == 10);
    REQUIRE(rec.errorPackets == 2);
    REQUIRE(rec.recoveredPackets == 5);
    REQUIRE(rec.recoveryRate() == Approx(50.0));
}

TEST_CASE("damage: 多段失败产生多个区间", "[damage]") {
    DamageLocator loc;
    for (int64_t i = 0; i < 20; ++i) {
        const bool ok = !(i == 2 || i == 15);
        loc.onPacket(ok, i, i * 50, i * 20.0);
    }
    loc.finish(false);
    REQUIRE(loc.ranges().size() == 2);
    REQUIRE(loc.ranges()[0].startPacket == 2);
    REQUIRE(loc.ranges()[1].startPacket == 15);
}

TEST_CASE("damage: 截断保留开放区间并归类 truncated", "[damage]") {
    DamageLocator loc;
    for (int64_t i = 0; i < 5; ++i) loc.onPacket(true, i, i * 10, i * 10.0);
    loc.onError({ErrorCategory::Truncated, 5, 50, 50.0, "EOF 提前"});
    loc.finish(true);
    REQUIRE(loc.ranges().size() == 1);
    REQUIRE(loc.ranges().front().category == ErrorCategory::Truncated);
}

TEST_CASE("damage: 无损坏时区间为空、恢复率为零分母安全", "[damage]") {
    DamageLocator loc;
    loc.finish(false);
    REQUIRE(loc.ranges().empty());
    REQUIRE(loc.recovery().recoveryRate() == 0.0);
}
