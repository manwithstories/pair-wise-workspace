#include <catch2/catch_test_macros.hpp>

#include <future>
#include <thread>

#include "stream.hpp"
#include "subtitle.hpp"
#include "timebase.hpp"
#include "timeline.hpp"

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

TEST_CASE("subtitle: SRT parse, offsets and error continuation",
          "[subtitle]") {
  // 注意每条之间空行；第二条时间非法，应跳过但后续仍被解析。
  std::string srt =
      "1\r\n00:00:01,000 --> 00:00:02,000\r\nhello\r\n\r\n"
      "2\r\n00:00:03,000 --> 00:00:01,000\r\nbad order\r\n\r\n"
      "3\r\n00:00:04,000 --> 00:00:05,000\r\nworld\r\n";
  auto doc = parse_subtitles(srt, SubFormat::kSrt);
  REQUIRE(doc.cues.size() == 2);
  REQUIRE(doc.skipped_count == 1);
  REQUIRE(doc.cues[0].text == "hello");
  REQUIRE(doc.cues[0].start_ticks == ms_to_master(1000));
  REQUIRE(doc.cues[1].text == "world");  // 错误后继续
  // 源偏移：第一条时间轴行位于序号行 "1\r\n" 之后（偏移 3）。
  REQUIRE(doc.cues[0].source_offset == 3);
  REQUIRE(doc.errors.size() >= 1);
  REQUIRE(doc.errors.front().code == kErrSubTime);
  REQUIRE(doc.errors.front().source_offset >= 0);
}

TEST_CASE("subtitle: VTT headers, overlap and duplicate", "[subtitle]") {
  std::string vtt =
      "WEBVTT\n\n"
      "NOTE a comment block\nspanning lines\n\n"
      "cue-id-1\n00:00:01.000 --> 00:00:03.000\nfirst\n\n"
      "00:00:02.000 --> 00:00:02.500\noverlap\n\n"
      "00:00:02.000 --> 00:00:02.500\ndup\n";
  auto doc = parse_subtitles(vtt, SubFormat::kVtt);
  REQUIRE(doc.cues.size() == 3);
  REQUIRE(doc.overlap_count == 1);
  REQUIRE(doc.duplicate_count == 1);
  bool has_ov = false, has_dup = false;
  for (auto& e : doc.errors) {
    has_ov |= e.code == kErrSubOverlap;
    has_dup |= e.code == kErrSubDuplicate;
  }
  REQUIRE(has_ov);
  REQUIRE(has_dup);
  REQUIRE(doc.cues[0].text == "first");
}

TEST_CASE("subtitle: truncated line and bounded text length", "[subtitle]") {
  std::string srt = "1\n00:00:01,000 --> 00:00:02,000\nok\n\n"
                    "2\n00:00:03,000 -->\n\n";  // 截断的时间轴
  auto doc = parse_subtitles(srt, SubFormat::kSrt);
  REQUIRE(doc.cues.size() == 1);
  bool trunc = false;
  for (auto& e : doc.errors) trunc |= e.code == kErrSubTruncated;
  REQUIRE(trunc);
}


TEST_CASE("stream: bounded capacity enforces backpressure", "[stream]") {
  constexpr size_t kCap = 4;
  BoundedQueue q(kCap);
  REQUIRE(q.capacity() == kCap);
  // 单线程预填到容量上限，峰值不得超过容量。
  for (int i = 0; i < 20; ++i) {
    std::vector<uint8_t> blob(16, static_cast<uint8_t>(i));
    size_t depth = 0;
    // 填满后需由消费者释放；这里用异步线程在满时取走一个。
    if (i >= static_cast<int>(kCap)) {
      auto fut = std::async(std::launch::async, [&] {
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
        auto v = q.pop();
        REQUIRE(v.has_value());
      });
      REQUIRE(q.push(std::move(blob), &depth));
      fut.wait();
    } else {
      REQUIRE(q.push(std::move(blob), &depth));
    }
    REQUIRE(depth <= kCap);
  }
  REQUIRE(q.peak_items() <= kCap);
  REQUIRE(q.peak_bytes() > 0);
  REQUIRE(q.total_pushed() == 20);
  REQUIRE(q.push_wait_count() >= 1);  // 确有背压等待发生
}

TEST_CASE("stream: close drains remaining items then ends", "[stream]") {
  BoundedQueue q(2);
  q.push({1, 2, 3});
  q.push({4});
  q.close();
  int got = 0;
  while (auto v = q.pop()) ++got;
  REQUIRE(got == 2);  // 关闭前排空，不丢数据
}

TEST_CASE("stream: capacity clamped to hard limit", "[stream]") {
  BoundedQueue huge(1'000'000);
  REQUIRE(huge.capacity() == kMaxQueueCapacity);
}

TEST_CASE("timeline: invalid clips rejected with reasons", "[timeline]") {
  std::vector<TrackSpec> tracks(1);
  tracks[0].id = "t0";
  tracks[0].priority = 1;
  tracks[0].boundary_start = 10;
  tracks[0].boundary_end = 100;
  tracks[0].clips = {
      {"a", -5, 10, 7},      // 负起点
      {"b", 20, 20, 8},      // 零长度
      {"c", 0, 30, 9},       // 起点早于轨边界
      {"d", 50, 120, 10},    // 尾部越界 -> 裁到 100
  };
  auto tl = compose_timeline(tracks);
  REQUIRE(tl.rejected == 3);
  // 最后一条裁尾后保留 [50,100)。
  bool kept = false;
  for (auto& s : tl.segments)
    if (s.source_ref == "d") { kept = true; REQUIRE(s.end == 100); }
  REQUIRE(kept);
  bool neg = false, zlen = false, oob = false;
  for (auto& d : tl.decisions) {
    neg |= d.code == kTlNegative;
    zlen |= d.code == kTlZeroLen;
    oob |= d.code == kTlOutOfBounds;
  }
  REQUIRE(neg); REQUIRE(zlen); REQUIRE(oob);
}

TEST_CASE("timeline: priority and source-order overlap rules", "[timeline]") {
  std::vector<TrackSpec> tracks(2);
  tracks[0].id = "low"; tracks[0].priority = 1;
  tracks[0].clips = {{"L", 0, 100, 1}};
  tracks[1].id = "high"; tracks[1].priority = 5;
  tracks[1].clips = {{"H", 40, 60, 2}};
  auto tl = compose_timeline(tracks);
  // 高优先级占据 [40,60)；低优先级被切成两段，共 3 段且互不重叠。
  REQUIRE(tl.segments.size() == 3);
  for (size_t i = 1; i < tl.segments.size(); ++i)
    REQUIRE(tl.segments[i].start >= tl.segments[i - 1].end);
  bool lowCliped = false;
  for (auto& d : tl.decisions) lowCliped |= d.code == kTlOverlapLow;
  REQUIRE(lowCliped);

  // 同优先级：先到先得（输入顺序），后者被标记 tie。
  std::vector<TrackSpec> tie(2);
  tie[0].id = "first"; tie[0].priority = 3; tie[0].clips = {{"A", 0, 10, 0}};
  tie[1].id = "second"; tie[1].priority = 3; tie[1].clips = {{"B", 5, 15, 0}};
  auto tl2 = compose_timeline(tie);
  bool tieMark = false;
  for (auto& d : tl2.decisions) tieMark |= d.code == kTlOverlapTie;
  REQUIRE(tieMark);
  REQUIRE(tl2.segments.size() == 2);  // [0,10) first + [10,15) second
}

TEST_CASE("timeline: empty track and global end", "[timeline]") {
  std::vector<TrackSpec> tracks(2);
  tracks[0].id = "empty"; tracks[0].priority = 1;
  tracks[1].id = "p"; tracks[1].priority = 1;
  tracks[1].clips = {{"x", 0, 1000, 0}};
  auto tl = compose_timeline(tracks, 200);
  bool emptyMark = false;
  for (auto& d : tl.decisions) emptyMark |= d.code == kTlEmptyTrack;
  REQUIRE(emptyMark);
  REQUIRE(tl.segments.size() == 1);
  REQUIRE(tl.segments[0].end == 200);  // 全局终点裁尾
}
