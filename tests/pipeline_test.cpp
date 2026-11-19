// pipeline_test.cpp — 批次 1 冒烟测试，后续批次扩展恢复/损坏/边界路径
#include <catch2/catch_test_macros.hpp>
#include "sha256.hpp"
#include "util.hpp"
#include "common.hpp"

using namespace md;

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
