#include "media/av_sync.hpp"
#include "media/checkpoint_index.hpp"
#include "media/demo_generator.hpp"
#include "media/jitter_buffer.hpp"
#include "media/packet_reorder.hpp"
#include "media/pipeline.hpp"
#include "media/recovery.hpp"
#include "media/subtitle.hpp"
#include "media/timebase.hpp"

#include <catch2/catch_approx.hpp>
#include <catch2/catch_test_macros.hpp>

#include <fstream>

using namespace media;
using Catch::Approx;

TEST_CASE("timebase: frame rates and day wrap", "[timebase]") {
    REQUIRE(frame_to_ticks(30, kFps30) == ms_to_ticks(1000));
    REQUIRE(frame_to_ticks(30000, kFps2997) == ms_to_ticks(1001000));
    // 日界回绕：23:59:59:29 -> 00:00:00:00 帧号单调递增
    FrameUnwrapper uw(kFps30);
    int64_t mod = frames_per_day(kFps30);
    int64_t a = uw.unwrap(mod - 1);
    int64_t b = uw.unwrap(0);
    REQUIRE(b == a + 1);
    // 重复时间戳被记录
    ErrorModel em;
    FrameUnwrapper uw2(kFps25);
    uw2.unwrap(100, &em, "v");
    uw2.unwrap(100, &em, "v");
    REQUIRE(em.count_of(ErrorKind::DuplicateTimestamp) == 1);
    // 不可换算项记录源轨道与错误类型
    ErrorModel em2;
    auto bad = timecode_to_ticks(Timecode{0, 0, 0, 99}, kFps24, &em2, "v1");
    REQUIRE(!bad.has_value());
    REQUIRE(em2.count_of(ErrorKind::UnconvertibleTimecode) == 1);
    REQUIRE(em2.records().front().source_track == "v1");
    // 丢帧时间码往返
    auto tc = Timecode{0, 1, 0, 2}; // 丢帧制式下 00:01:00;02 为合法首帧
    auto f = timecode_to_frame(tc, kFps2997);
    REQUIRE(f.has_value());
}

TEST_CASE("reorder: dedup, window, late packets", "[reorder]") {
    ErrorModel em;
    PacketReorder r(4);
    MediaPacket p;
    p.sequence = 0;
    REQUIRE(r.push(p, em).size() == 1);
    // 重复包（序号+时间戳一致）
    REQUIRE(r.push(p, em).empty());
    REQUIRE(r.duplicates() == 1);
    // 乱序窗口内重排：2,1,3
    p.sequence = 2;
    REQUIRE(r.push(p, em).empty());
    p.sequence = 1;
    auto out = r.push(p, em);
    REQUIRE(out.size() == 2);
    REQUIRE(out[0].sequence == 1);
    p.sequence = 3;
    REQUIRE(r.push(p, em).size() == 1);
    // 窗口外迟到包：不回退已提交
    p.sequence = 0;
    REQUIRE(r.push(p, em).empty());
    // 序号缺口超窗 -> 记丢失并推进（窗口 4，积压 5 个触发判定）
    for (uint64_t s = 20; s <= 24; ++s) {
        p.sequence = s;
        r.push(p, em);
    }
    REQUIRE(em.count_of(ErrorKind::Lost) > 0);
}

TEST_CASE("jitter buffer adapts and survives bandwidth drop", "[jitter]") {
    JitterBuffer jb(1000, 8, 256, 120);
    int64_t t = 0;
    for (int i = 0; i < 200; ++i) {
        t += 8;
        jb.observe(t, 1200);
    }
    uint32_t depth_normal = jb.target_depth();
    REQUIRE(depth_normal >= 8);
    // 带宽突降：吞吐骤降时应保留可恢复数据（深度只增不减）
    uint32_t depth_before = jb.target_depth();
    for (int i = 0; i < 200; ++i) {
        t += 80;
        auto snap = jb.observe(t, 1200);
        (void)snap;
    }
    REQUIRE(jb.target_depth() >= depth_before);
    REQUIRE(jb.max_wait_ms() == 120);
}

TEST_CASE("recovery: FEC recovers bursts, marks gaps otherwise", "[recovery]") {
    ErrorModel em;
    RecoveryStats st;
    std::vector<MediaPacket> present;
    for (uint64_t s = 0; s < 8; ++s) {
        if (s == 3 || s == 4) continue; // 丢 2 个
        MediaPacket p;
        p.sequence = s;
        p.pts_ticks = static_cast<int64_t>(s) * 100;
        p.payload_bytes = 1200;
        present.push_back(p);
    }
    auto rec = recover_group(0, present, 2, 8, st, em);
    REQUIRE(rec.recovered.size() == 2);
    REQUIRE(rec.gaps.empty());
    REQUIRE(st.recovery_rate() == Approx(1.0));
    // 冗余不足：丢 3 个只有 2 个校验 -> 明确缺口，不伪造样本
    RecoveryStats st2;
    std::vector<MediaPacket> p2(present.begin(), present.end() - 1);
    auto rec2 = recover_group(0, p2, 2, 8, st2, em);
    REQUIRE(rec2.recovered.empty());
    REQUIRE(!rec2.gaps.empty());
    REQUIRE(st2.unrecoverable_packets == 3);
}

TEST_CASE("subtitle: corrupt/truncated tolerated, overlap trimmed", "[subtitle]") {
    ErrorModel em;
    std::string srt =
        "1\n00:00:01,000 --> 00:00:02,000\nhello\n\n"
        "2\nBAD-TIMING\nbroken\n\n"
        "3\n00:00:03,000 --> 00:00:04,000\nworld\n\n"
        "4\n00:00:05,000 --> 00:00:06,000\n"; // 截断：无正文
    auto r = parse_subtitles(srt, SubtitleFormat::Srt, OverlapPolicy::Keep, em, "s");
    REQUIRE(r.cues.size() == 3); // 坏记录跳过，截断记录保留时间区间
    REQUIRE(r.bad_records == 2);
    REQUIRE(em.count_of(ErrorKind::CorruptRecord) == 1);
    REQUIRE(em.count_of(ErrorKind::TruncatedRecord) == 1);
    bool has_line = false;
    for (const auto& e : em.records())
        if (e.line > 0 && e.offset >= 0) has_line = true;
    REQUIRE(has_line);
    // WebVTT 重叠裁剪
    std::string vtt =
        "WEBVTT\n\n00:00.500 --> 00:02.000\na\n\n00:01.500 --> 00:03.000\nb\n\n";
    auto r2 = parse_subtitles(vtt, SubtitleFormat::WebVtt, OverlapPolicy::Trim, em, "v");
    REQUIRE(r2.cues.size() == 2);
    REQUIRE(r2.cues[0].end_ticks <= r2.cues[1].start_ticks);
}

TEST_CASE("checkpoint: commit, resume, corrupt rollback", "[checkpoint]") {
    auto dir = std::filesystem::temp_directory_path() / "mb_test_ckpt";
    std::filesystem::remove_all(dir);
    ErrorModel em;
    std::vector<MediaPacket> pkts;
    for (uint64_t i = 0; i < 8; ++i) {
        MediaPacket p;
        p.sequence = i;
        p.payload_bytes = 100;
        pkts.push_back(p);
    }
    {
        CheckpointIndex idx(dir, 8);
        REQUIRE(idx.load(em));
        REQUIRE(idx.commit_chunk(pkts, em));
        REQUIRE(idx.commit_chunk(pkts, em));
        REQUIRE(idx.chunk_count() == 2);
    }
    {
        CheckpointIndex idx(dir, 8);
        REQUIRE(idx.load(em));
        REQUIRE(idx.chunk_count() == 2);
        REQUIRE(idx.resume_sequence() == 8);
        REQUIRE(idx.committed_packets() == 16);
    }
    // 损坏第二块 -> 回退到最近有效块并报告
    {
        auto f = dir / "chunk_000001.idx";
        std::ofstream o(f);
        o << "garbage\n";
        o.close();
        CheckpointIndex idx(dir, 8);
        idx.load(em);
        REQUIRE(idx.chunk_count() == 1);
        REQUIRE(em.count_of(ErrorKind::CheckpointCorrupt) >= 1);
    }
    std::filesystem::remove_all(dir);
}

TEST_CASE("av sync: offset, drift, resample", "[avsync]") {
    AvSync s;
    for (int i = 0; i < 100; ++i) {
        int64_t v = i * 9000;
        s.add_pair(v + 1080 + i, v); // +12ms 偏移 + 线性漂移
    }
    auto r = s.report();
    REQUIRE(r.offset_ms == Approx(12.0).margin(0.5));
    REQUIRE(r.residual_error_ms < 1.0);
    std::vector<int16_t> tone(44100, 500);
    auto out = resample_s16_mono(tone, 44100, 48000);
    REQUIRE(out.size() > 47000);
    REQUIRE(out.size() < 49000);
}

TEST_CASE("pipeline end-to-end with interruption and resume", "[pipeline]") {
    auto dir = std::filesystem::temp_directory_path() / "mb_test_pipe";
    std::filesystem::remove_all(dir);
    DemoConfig cfg;
    cfg.seed = 41;
    cfg.bandwidth_kbps = 3200;
    cfg.target_kbps = 1800;
    cfg.checkpoint_packets = 256;
    cfg.output_dir = dir.string();
    ErrorModel em;
    DemoTrace trace = generate_demo_trace(cfg);
    REQUIRE(trace.total_data_packets > 0);
    REQUIRE(trace.lost_in_channel > 0);
    // 中断后续跑：两次完成同一条轨迹
    auto r1 = run_pipeline(cfg, trace, em, trace.arrivals.size() / 2);
    REQUIRE(!r1.completed);
    auto r2 = run_pipeline(cfg, trace, em, UINT64_MAX);
    REQUIRE(r2.completed);
    REQUIRE(r2.resumed);
    REQUIRE(r2.manifest.recovered_packets > 0);
    REQUIRE(r2.manifest.unrecoverable_packets == 0);
    REQUIRE(std::abs(r2.manifest.bitrate_delta_pct) <= 8.0);
    REQUIRE(r2.manifest.quality_psnr_db >= 30.0);
    REQUIRE(r2.manifest.manifest_max_error_ms <= 20.0);
    // 检查点文件与清单已生成
    REQUIRE(std::filesystem::exists(
        std::filesystem::path(dir) / "manifest.json"));
    std::filesystem::remove_all(dir);
}
