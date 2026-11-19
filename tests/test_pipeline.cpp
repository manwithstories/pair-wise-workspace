// 损坏定位与流式管线状态/边界路径单测。
#include "damage_locator.hpp"
#include "stream_pipeline.hpp"
#include <catch2/catch_test_macros.hpp>
#include <catch2/catch_approx.hpp>
#include <atomic>
#include <thread>

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

TEST_CASE("pipeline: 有界队列背压与取消", "[pipeline]") {
    BoundedQueue<int> q(/*maxBytes*/ 128); // 每项 64B → 最多缓存 2 项
    CancellationToken token;
    int backpressure = 0; // 仅生产者写；join 后读取，线程安全
    std::thread producer([&] {
        for (int i = 0; i < 4; ++i)
            REQUIRE(q.push(i, 64, token, &backpressure));
        q.close();
    });
    for (int i = 0; i < 4; ++i) {
        auto v = q.pop(token);
        REQUIRE(v.has_value());
        REQUIRE(*v == i);
    }
    producer.join();
    REQUIRE(backpressure >= 1); // 队列满过 → 发生背压
    REQUIRE(!q.pop(token).has_value());
}

TEST_CASE("pipeline: 取消立即解除阻塞且传播错误", "[pipeline]") {
    BoundedQueue<int> q(64);
    PipelineErrorState err;
    int backpressure = 0;
    REQUIRE(q.push(1, 64, err.token(), &backpressure)); // 队列已满
    std::atomic<bool> returned{false};
    std::thread blocked([&] {
        bool ok = q.push(2, 64, err.token(), &backpressure);
        returned.store(ok);
    });
    std::this_thread::sleep_for(std::chrono::milliseconds(30));
    REQUIRE(!returned.load()); // 满队列阻塞中
    err.fail("上游致命错误");
    blocked.join();
    REQUIRE(!returned.load()); // push 返回 false
    REQUIRE(err.failed());
    REQUIRE(err.message() == "上游致命错误");
}

TEST_CASE("pipeline: 延迟 P95 与 RSS 采样可用", "[pipeline]") {
    PipelineMetrics m;
    for (double v : {1.0, 2.0, 3.0, 4.0, 100.0}) m.latencyMs.push_back(v);
    REQUIRE(m.latencyP95Ms() == Approx(80.8));
    const double rss = currentRssMiB();
    REQUIRE(rss > 0.0);
    REQUIRE(rss < 512.0); // 测试进程自身不应超过 512 MiB
}
