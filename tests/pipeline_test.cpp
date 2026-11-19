// pipeline_test.cpp — 覆盖恢复、损坏和边界路径
#include <catch2/catch_test_macros.hpp>
#include "sha256.hpp"
#include "util.hpp"
#include "common.hpp"
#include "packet_source.hpp"
#include "timeline.hpp"
#include "segmenter.hpp"

using namespace md;

namespace {
std::string tmp_path(const char* name) {
    return std::string("/tmp/mdtest_") + name + "_" +
           std::to_string(uintptr_t(&name)) + ".rtp";
}
} // namespace

TEST_CASE("sha256 deterministic and known vector", "[smoke]") {
    auto d = Sha256::hash_bytes(reinterpret_cast<const uint8_t*>("abc"), 3);
    REQUIRE(Sha256::hex(d) ==
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    auto d2 = Sha256::hash_bytes(reinterpret_cast<const uint8_t*>("abc"), 3);
    REQUIRE(d == d2);
}

TEST_CASE("wrap arithmetic handles 16/32 bit rollover", "[smoke]") {
    REQUIRE(wrap_delta(5, 65530, SEQ_MOD) == 11);
    REQUIRE(wrap_signed_delta(5, 65530, SEQ_MOD) == 11);
    REQUIRE(wrap_signed_delta(65530, 5, SEQ_MOD) == -11);
    REQUIRE(wrap_delta(10u, 0xFFFFFFFAu, TS_MOD) == 16u);
}

TEST_CASE("capture generation is deterministic and readable", "[source]") {
    auto p1 = tmp_path("det1"), p2 = tmp_path("det2");
    GenerateOptions o; o.seed = 41; o.loss = 0.02; o.reorder = 0.08;
    PipelineError e1, e2;
    REQUIRE(generate_capture(p1, o, e1) > 0);
    REQUIRE(generate_capture(p2, o, e2) > 0);
    PacketSource s1, s2;
    REQUIRE(s1.open(p1, e1));
    REQUIRE(s2.open(p2, e2));
    REQUIRE(s1.summary().sha256 == s2.summary().sha256); // 同参同输出
    uint32_t n = 0; MediaPacket pkt;
    while (s1.next_rtp(pkt, e1)) ++n;
    REQUIRE(e1.type == ErrType::None);
    REQUIRE(n > 50);
    REQUIRE_FALSE(s1.rtcp_reports().empty());
}

TEST_CASE("truncated capture is reported with traceable error", "[source][corrupt]") {
    auto p = tmp_path("trunc");
    GenerateOptions o; o.seed = 7; o.truncated = true;
    PipelineError e;
    REQUIRE(generate_capture(p, o, e) > 0);
    PacketSource s;
    REQUIRE(s.open(p, e));
    MediaPacket pkt;
    while (s.next_rtp(pkt, e)) {}
    REQUIRE(e.type == ErrType::TruncatedRecord);
    REQUIRE(e.stage == stage::Source);
}

TEST_CASE("wrap sample drives seq and ts across boundaries", "[source][wrap]") {
    auto p = tmp_path("wrap");
    GenerateOptions o; o.seed = 9; o.wrap_mode = 1;
    PipelineError e;
    REQUIRE(generate_capture(p, o, e) > 0);
    PacketSource s;
    REQUIRE(s.open(p, e));
    bool saw_low_after_high = false;
    uint16_t prev = 0; bool have = false;
    MediaPacket pkt;
    uint32_t n = 0;
    while (s.next_rtp(pkt, e)) {
        if (pkt.codec == CodecId::H264) {
            if (have && pkt.seq < prev && prev > 60000) saw_low_after_high = true;
            prev = pkt.seq; have = true;
        }
        ++n;
    }
    REQUIRE(e.type == ErrType::None);
    REQUIRE(saw_low_after_high);
    REQUIRE(n > 0);
}

TEST_CASE("aac and pcm captures both decode as supported codecs", "[source]") {
    for (bool aac : {false, true}) {
        auto p = tmp_path(aac ? "aac" : "pcm");
        GenerateOptions o; o.seed = 11; o.aac = aac;
        PipelineError e;
        REQUIRE(generate_capture(p, o, e) > 0);
        PacketSource s;
        REQUIRE(s.open(p, e));
        MediaPacket pkt; bool saw_audio = false;
        while (s.next_rtp(pkt, e)) {
            if (pkt.codec == (aac ? CodecId::Aac : CodecId::PcmS16)) {
                saw_audio = true;
                if (aac) REQUIRE(pkt.payload.size() >= 7); // ADTS 头
            }
        }
        REQUIRE(e.type == ErrType::None);
        REQUIRE(saw_audio);
    }
}

TEST_CASE("bad magic is rejected", "[source][corrupt]") {
    auto p = tmp_path("magic");
    std::vector<uint8_t> junk(128, 0xAB);
    std::string errmsg;
    REQUIRE(atomic_write(p, junk, errmsg));
    PacketSource s;
    PipelineError e;
    REQUIRE_FALSE(s.open(p, e));
    REQUIRE(e.type == ErrType::BadMagic);
}


// ---- 时间轴归一化 ----
namespace {
MediaPacket mk_pkt(uint32_t ssrc, uint16_t seq, uint32_t ts,
                   uint32_t clock = 90000, CodecId codec = CodecId::H264) {
    MediaPacket p;
    p.ssrc = ssrc; p.seq = seq; p.rtp_ts = ts;
    p.clock_rate = clock; p.codec = codec;
    p.payload.push_back(uint8_t(seq & 0xFF));
    return p;
}
} // namespace

TEST_CASE("seq and ts wraparound extend monotonically", "[timeline][wrap]") {
    ProcessConfig cfg;
    TimelineNormalizer tn(cfg);
    std::vector<MediaPacket> out;
    std::vector<PipelineError> errs;
    uint32_t ts = 0xFFFFFFF0u;
    for (uint16_t s = 65532; s != 4u; ++s) { // 65532..65535, 0..3
        tn.push(mk_pkt(1, s, ts), out, errs);
        ts += 3000; // 自然越过 32 位边界
    }
    tn.flush(out, errs);
    REQUIRE(out.size() == 8);
    for (size_t i = 1; i < out.size(); ++i) {
        REQUIRE(out[i].unified_us > out[i - 1].unified_us);
        REQUIRE(out[i].untrusted == false);
    }
}

TEST_CASE("reordering is restored and duplicates flagged", "[timeline]") {
    ProcessConfig cfg;
    TimelineNormalizer tn(cfg);
    std::vector<MediaPacket> out;
    std::vector<PipelineError> errs;
    tn.push(mk_pkt(1, 0, 0), out, errs);
    tn.push(mk_pkt(1, 2, 6000), out, errs);   // 乱序：2 先到
    tn.push(mk_pkt(1, 1, 3000), out, errs);   // 1 后到
    tn.push(mk_pkt(1, 1, 3000), out, errs);   // 重复
    tn.flush(out, errs);
    REQUIRE(out.size() == 4);
    REQUIRE(out[0].seq == 0);
    REQUIRE(out[1].seq == 1);
    REQUIRE(out[2].seq == 2);
    REQUIRE(out[3].duplicate == true);
    REQUIRE(tn.counters().duplicates == 1);
    REQUIRE(tn.counters().reordered >= 1);
}

TEST_CASE("packet loss opens untrusted span that closes on realignment",
          "[timeline][corrupt]") {
    ProcessConfig cfg;
    TimelineNormalizer tn(cfg);
    std::vector<MediaPacket> out;
    std::vector<PipelineError> errs;
    tn.push(mk_pkt(1, 10, 0), out, errs);
    tn.push(mk_pkt(1, 14, 12000), out, errs); // 缺 11,12,13
    tn.flush(out, errs);
    REQUIRE(tn.counters().losses == 3);
    REQUIRE(tn.counters().untrusted_spans == 1);
    bool has_untrusted_err = false;
    for (auto& e : errs) has_untrusted_err |= e.type == ErrType::UntrustedTime;
    REQUIRE(has_untrusted_err);
    REQUIRE(out.back().untrusted == true);
    // 不伪造连续媒体：交付包数仍为 2
    REQUIRE(out.size() == 2);
}

TEST_CASE("rtcp drift beyond limit retains raw time and records source",
          "[timeline]") {
    ProcessConfig cfg;
    cfg.drift_limit_us = 1000;
    TimelineNormalizer tn(cfg);
    std::vector<MediaPacket> out;
    std::vector<PipelineError> errs;
    tn.apply_rtcp({1, 0, 0}, errs);                 // 锚点
    tn.push(mk_pkt(1, 0, 0), out, errs);
    // 同一 rtp_ts 却报告晚了 50ms -> 漂移 50000us
    tn.apply_rtcp({1, 9000, 50'000 + 1'000}, errs);
    bool drift_err = false;
    for (auto& e : errs) drift_err |= e.type == ErrType::ClockDrift;
    REQUIRE(drift_err);
    REQUIRE(tn.counters().drift_violations == 1);
}

TEST_CASE("end-to-end capture normalizes with bounded loss accounting",
          "[timeline][source]") {
    auto p = tmp_path("e2e");
    GenerateOptions o; o.seed = 41; o.loss = 0.05; o.reorder = 0.1;
    PipelineError e;
    REQUIRE(generate_capture(p, o, e) > 0);
    PacketSource src;
    REQUIRE(src.open(p, e));
    ProcessConfig cfg;
    TimelineNormalizer tn(cfg);
    std::vector<MediaPacket> norm;
    std::vector<PipelineError> errs;
    MediaPacket pkt;
    while (src.next_rtp(pkt, [&](const RtcpReport& rr) { tn.apply_rtcp(rr, errs); }, e))
        tn.push(pkt, norm, errs);
    tn.flush(norm, errs);
    REQUIRE(e.type == ErrType::None);
    REQUIRE(norm.size() > 50);
    // 统一时间基准：同 SSRC 内非重复包时间单调
    std::map<uint32_t, int64_t> last;
    for (auto& q : norm) {
        if (q.duplicate) continue;
        auto it = last.find(q.ssrc);
        if (it != last.end()) REQUIRE(q.unified_us >= it->second);
        last[q.ssrc] = q.unified_us;
    }
}

// ---- 片段切分 ----
namespace {
// 构造带起始码的 H.264 NAL 包：nal 5=IDR(关键帧), 1=P 帧
MediaPacket mk_video(uint16_t seq, int64_t us, unsigned nal,
                     bool untrusted = false) {
    MediaPacket p;
    p.ssrc = 0x10000001; p.seq = seq; p.codec = CodecId::H264;
    p.clock_rate = 90000; p.unified_us = us; p.untrusted = untrusted;
    p.payload = {0x00, 0x00, 0x00, 0x01, uint8_t(nal & 0x1F)};
    return p;
}
} // namespace

TEST_CASE("segments start on keyframes and index correctly", "[segment]") {
    ProcessConfig cfg; cfg.min_segment_us = 100'000; cfg.max_frame_cache = 64;
    Segmenter seg(cfg);
    std::vector<ClosedSegment> out;
    std::vector<SilenceInterval> no_sil;
    auto emit = [&](ClosedSegment s) { out.push_back(std::move(s)); };
    // IDR,P,P, IDR,P,P  每个关键帧间隔 200ms
    int64_t t = 0; uint16_t sq = 0;
    for (int g = 0; g < 3; ++g) {
        seg.push_video(mk_video(sq++, t, 5), no_sil, emit); t += 100'000;
        seg.push_video(mk_video(sq++, t, 1), no_sil, emit); t += 100'000;
        seg.push_video(mk_video(sq++, t, 1), no_sil, emit); t += 100'000;
    }
    seg.finish(no_sil, emit);
    REQUIRE(out.size() == 3);
    for (auto& s : out) REQUIRE(s.starts_with_keyframe);
    REQUIRE(seg.counters().keyframe_indexed == 3);
    REQUIRE(seg.counters().missing_keyframe == 0);
}

TEST_CASE("missing keyframe yields explicit disposition", "[segment][corrupt]") {
    ProcessConfig cfg; cfg.min_segment_us = 100'000;
    Segmenter seg(cfg);
    std::vector<ClosedSegment> out;
    std::vector<SilenceInterval> no_sil;
    auto emit = [&](ClosedSegment s) { out.push_back(std::move(s)); };
    // 全程 P 帧，无关键帧
    for (int i = 0; i < 4; ++i)
        seg.push_video(mk_video(uint16_t(i), int64_t(i) * 100'000, 1), no_sil, emit);
    seg.finish(no_sil, emit);
    REQUIRE(out.size() == 1);
    REQUIRE(out[0].disposition == Disposition::MissingKeyframe);
    REQUIRE(seg.counters().missing_keyframe == 1);
}

TEST_CASE("short segment below minimum is flagged", "[segment]") {
    ProcessConfig cfg; cfg.min_segment_us = 500'000;
    Segmenter seg(cfg);
    std::vector<ClosedSegment> out;
    std::vector<SilenceInterval> no_sil;
    auto emit = [&](ClosedSegment s) { out.push_back(std::move(s)); };
    seg.push_video(mk_video(0, 0, 5), no_sil, emit);
    seg.push_video(mk_video(1, 50'000, 1), no_sil, emit);
    seg.finish(no_sil, emit);
    REQUIRE(out.size() == 1);
    REQUIRE(out[0].disposition == Disposition::ShortSegment);
}

TEST_CASE("silence crossing packet boundary is reported", "[segment]") {
    ProcessConfig cfg; cfg.min_segment_us = 100'000;
    Segmenter seg(cfg);
    std::vector<ClosedSegment> out;
    // 静音区间跨过第二个包的时间点
    std::vector<SilenceInterval> sil{{150'000, 450'000}};
    auto emit = [&](ClosedSegment s) { out.push_back(std::move(s)); };
    seg.push_video(mk_video(0, 0, 5), sil, emit);
    seg.push_video(mk_video(1, 200'000, 1), sil, emit);
    seg.push_video(mk_video(2, 300'000, 1), sil, emit);
    seg.finish(sil, emit);
    REQUIRE(out.size() == 1);
    REQUIRE(out[0].silence_cross_boundary);
    REQUIRE(out[0].disposition == Disposition::SilenceCrossBoundary);
}

TEST_CASE("frame cache is bounded regardless of input length", "[segment]") {
    ProcessConfig cfg; cfg.max_frame_cache = 8; cfg.min_segment_us = 1;
    Segmenter seg(cfg);
    std::vector<ClosedSegment> out;
    std::vector<SilenceInterval> no_sil;
    auto emit = [&](ClosedSegment s) { out.push_back(std::move(s)); };
    // 100 个 P 帧、无关键帧，缓存上限 8 => 强制多次闭合
    for (int i = 0; i < 100; ++i)
        seg.push_video(mk_video(uint16_t(i), int64_t(i) * 1000, 1), no_sil, emit);
    seg.finish(no_sil, emit);
    REQUIRE(out.size() >= 12);
    for (auto& s : out) REQUIRE(s.packets.size() <= 8);
}
