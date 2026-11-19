#include <catch2/catch_test_macros.hpp>

#include "timebase.hpp"

using namespace mb;

// 骨架测试：验证 Catch2 与构建链可用，后续批次追加真实用例。
TEST_CASE("skeleton: test runner alive", "[skeleton]") {
  REQUIRE(1 + 1 == 2);
}

TEST_CASE("timebase: integer master ticks, no float drift", "[timebase]") {
  auto tb25 = Timebase::fps25();
  // 25 fps：一帧恰为 3600 个 90kHz tick。
  REQUIRE(frame_to_master(0, tb25) == 0);
  REQUIRE(frame_to_master(1, tb25) == 3600);
  REQUIRE(frame_to_master(25, tb25) == 90000);
  // 往返一致。
  for (int64_t fr : {1, 25, 1000, 123456})
    REQUIRE(master_to_frame(frame_to_master(fr, tb25), tb25) == fr);
  // NTSC 30000/1001：tick 精确（90000*1001/30000 = 3003）。
  auto ntsc = Timebase::ntsc30ndf();
  REQUIRE(frame_to_master(1, ntsc) == 3003);
  REQUIRE(master_to_frame(3003, ntsc) == 1);
}

TEST_CASE("timebase: drop-frame SMPTE round trip and illegal frames",
          "[timebase]") {
  auto df = Timebase::ntsc30();
  REQUIRE(df.drop_frame);
  // 00:00:00;00 起始；丢帧号 00:01:00;00 / ;01 非法。
  REQUIRE(timecode_to_frame("00:00:00;00", df).value() == 0);
  REQUIRE_FALSE(timecode_to_frame("00:01:00;00", df).has_value());
  REQUIRE_FALSE(timecode_to_frame("00:01:00;01", df).has_value());
  // 00:01:00;02 按 SMPTE 12M 正解为第 1800 帧（1802 名义帧减 2 跳过帧）。
  REQUIRE(timecode_to_frame("00:01:00;02", df).value() == 1800);
  // 反解：帧 1800 必须格式化为 00:01:00;02。
  REQUIRE(frame_to_timecode(1800, df) == "00:01:00;02");
  // 往返：大量帧号格式化后再解析必须回到原帧号。
  for (int64_t fr = 0; fr < 30 * 60 * 60; fr += 97) {
    auto tc = frame_to_timecode(fr, df);
    auto back = timecode_to_frame(tc, df);
    REQUIRE(back.has_value());
    REQUIRE(back.value() == fr);
  }
  // 非丢帧制式拒绝 ';' 标记，丢帧制式拒绝 ':'。
  auto ndf = Timebase::ntsc30ndf();
  REQUIRE_FALSE(timecode_to_frame("00:00:00;00", ndf).has_value());
  REQUIRE_FALSE(timecode_to_frame("00:00:00:00", df).has_value());
}

TEST_CASE("timebase: day wrap and millisecond timestamps", "[timebase]") {
  auto tb25 = Timebase::fps25();
  // 日界回绕：24:00:00:00 回到帧 0。
  REQUIRE(timecode_to_frame("24:00:00:00", tb25).value() == 0);
  REQUIRE(timecode_to_frame("24:00:00:01", tb25).value() == 1);
  // 毫秒时间戳（SRT 逗号 / VTT 点号 / 无小时 VTT）。
  REQUIRE(parse_ms_timestamp("00:00:01,000").value() == 90000);
  REQUIRE(parse_ms_timestamp("00:00:01.500").value() == 135000);
  REQUIRE(parse_ms_timestamp("01:02.250").value() ==
          ms_to_master(62250));
  REQUIRE_FALSE(parse_ms_timestamp("00:99:00,000").has_value());
  REQUIRE_FALSE(parse_ms_timestamp("garbage").has_value());
  REQUIRE(format_ms_timestamp(135000) == "00:00:01,500");
}

