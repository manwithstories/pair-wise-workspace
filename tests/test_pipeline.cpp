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

#include "sync_recovery.hpp"

TEST_CASE("sync: 完美同步误差为零", "[sync]") {
    SyncRecovery sr;
    for (int i = 0; i <= 100; ++i) {
        const double t = i * 40.0; // 25fps，ms
        sr.addVideo(t, t);
        sr.addAudio(t, t);
    }
    const auto r = sr.estimate();
    REQUIRE(r.sampleCount == 101);
    REQUIRE(r.errorAfterRmsMs == Approx(0.0).margin(1e-9));
    REQUIRE(r.driftPpm == Approx(0.0).margin(1e-6));
}

TEST_CASE("sync: 恒定音画偏移可被截距校正", "[sync]") {
    SyncRecovery sr;
    for (int i = 0; i <= 100; ++i) {
        const double t = i * 40.0;
        sr.addVideo(t + 12.0, t); // 视频恒定超前 12ms
        sr.addAudio(t, t);
    }
    const auto r = sr.estimate();
    REQUIRE(r.initialOffsetMs == Approx(12.0).margin(1e-6));
    REQUIRE(r.driftPpm == Approx(0.0).margin(1e-6));
    REQUIRE(r.errorAfterRmsMs == Approx(0.0).margin(1e-9));
}

TEST_CASE("sync: 渐进漂移 100ppm 被识别且校正后误差达标", "[sync]") {
    SyncRecovery sr;
    const double ppm = 100.0;
    for (int i = 0; i <= 250; ++i) { // 10 秒 @25fps
        const double t = i * 40.0;
        const double drift = t * ppm / 1e6; // ms
        sr.addVideo(t + drift, t);
        sr.addAudio(t, t);
    }
    const auto r = sr.estimate();
    REQUIRE(r.driftPpm == Approx(100.0).margin(0.5));
    REQUIRE(r.errorAfterP95Ms < 20.0);
    REQUIRE(r.errorBeforeP95Ms > r.errorAfterP95Ms);
}

TEST_CASE("sync: 校正不掩盖异常且保留原始偏移", "[sync]") {
    SyncRecovery sr;
    for (int i = 0; i <= 200; ++i) {
        const double t = i * 40.0;
        const double glitch = (i == 100) ? 80.0 : 0.0;
        sr.addVideo(t + glitch, t);
        sr.addAudio(t, t);
    }
    const auto r = sr.estimate();
    REQUIRE(r.anomalyCount >= 1);
    REQUIRE(r.rawOffsetsMs.size() == r.residualsMs.size());
    REQUIRE(r.rawOffsetsMs[100] == Approx(80.0)); // 原始观测未被修改
}

TEST_CASE("sync: 缺流/样本不足安全返回", "[sync]") {
    SyncRecovery sr;
    REQUIRE(sr.estimate().sampleCount == 0);
    sr.addVideo(0.0, 0.0);
    REQUIRE(sr.estimate().sampleCount == 0);
}
