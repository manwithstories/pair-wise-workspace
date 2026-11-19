// pipeline_test.cpp — 覆盖恢复、损坏和边界路径
#include <catch2/catch_test_macros.hpp>
#include "sha256.hpp"
#include "util.hpp"
#include "common.hpp"
#include "packet_source.hpp"

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

