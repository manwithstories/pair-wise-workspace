#include <catch2/catch_test_macros.hpp>
#include <atomic>
#include <chrono>
#include <fstream>
#include <thread>

#include "relay/analysis_engine.hpp"
#include "relay/bounded_pipeline.hpp"
#include "relay/caption_parser.hpp"
#include "relay/checkpoint_store.hpp"
#include "relay/media_time.hpp"
#include "relay/sha256.hpp"
#include "relay/transport_reorder.hpp"

using namespace relay;

TEST_CASE("rational arithmetic and ms conversion", "[time]") {
  auto r = make_rational(1001, 30000);
  REQUIRE(r.num == 1001);
  REQUIRE(r.den == 30000);
  REQUIRE(rational_floor(rational_mul({1500, 1}, r)) == 50);  // 50.05 -> 50
  REQUIRE(rational_round({7, 2}) == 4);
  REQUIRE(rational_round({-7, 2}) == -4);
}

TEST_CASE("fractional frame rate conversion reports error bounds", "[time]") {
  // 1001 ms at 30000/1001 fps lands exactly on frame 30.
  auto exact = ms_to_frame({1001, 1}, TimecodeStandard::NtscDrop30);
  REQUIRE(exact.exact);
  REQUIRE(exact.frame == 30);
  REQUIRE(exact.error_bound_ms.num == 0);
  // 1000 ms does not; the error bound must be one frame duration.
  auto inexact = ms_to_frame({1000, 1}, TimecodeStandard::NtscDrop30);
  REQUIRE_FALSE(inexact.exact);
  REQUIRE(inexact.frame == 29);
  REQUIRE(rational_cmp(inexact.error_bound_ms, {1001, 30}) == 0);
}

TEST_CASE("drop-frame timecode round trip", "[time]") {
  Timecode tc{0, 1, 0, 2, true};
  REQUIRE(timecode_to_frame(tc, TimecodeStandard::NtscDrop30) == 1800);
  auto back = frame_to_timecode(1800, TimecodeStandard::NtscDrop30);
  REQUIRE(back.mm == 1);
  REQUIRE(back.ff == 2);
  // 10-minute boundary: no drop.
  auto ten = frame_to_timecode(17982, TimecodeStandard::NtscDrop30);
  REQUIRE(ten.mm == 10);
  REQUIRE(ten.ff == 0);
}

TEST_CASE("reorder dedups, reorders and marks burst gaps", "[transport]") {
  TransportReorderer r(8, 3);
  std::vector<PacketRecord> out;
  auto pkt = [](uint64_t s) {
    PacketRecord p;
    p.seq = s;
    p.ts_ms = static_cast<int64_t>(s) * 40;
    return p;
  };
  REQUIRE(r.push(pkt(0), out) == ReorderEvent::InOrder);
  REQUIRE(r.push(pkt(0), out) == ReorderEvent::Duplicate);
  REQUIRE(r.push(pkt(2), out) == ReorderEvent::Reordered);
  REQUIRE(r.push(pkt(1), out) == ReorderEvent::InOrder);
  REQUIRE(out.size() == 3);
  // Burst loss of seq 3..6, then 7 arrives in-window.
  REQUIRE(r.push(pkt(7), out) == ReorderEvent::Reordered);
  r.flush(out);
  REQUIRE(r.stats().duplicate_packets == 1);
  REQUIRE(r.stats().reordered_packets == 2);
  REQUIRE(r.stats().lost_packets == 4);
  REQUIRE(r.stats().burst_losses == 1);
  REQUIRE(r.gaps().front().first_seq == 3);
  REQUIRE(r.gaps().front().last_seq == 6);
  REQUIRE(out.back().seq == 7);
}

TEST_CASE("late packet beyond window is recorded not resequenced", "[transport]") {
  TransportReorderer r(4, 3);
  std::vector<PacketRecord> out;
  for (uint64_t s = 0; s < 10; ++s) {
    PacketRecord p;
    p.seq = s;
    r.push(p, out);
  }
  PacketRecord late;
  late.seq = 2;  // window covers 6..9; 2 is far behind
  REQUIRE(r.push(late, out) == ReorderEvent::LateDropped);
  REQUIRE(r.stats().late_dropped == 1);
  REQUIRE(out.back().seq == 9);  // order untouched
}

TEST_CASE("srt parser keeps raw times, offsets and survives bad cues", "[caption]") {
  std::string text =
      "1\n00:00:01,000 --> 00:00:02,000\nhello\n\n"
      "2\n99:99:99,999 --> 99:99:99,999\nbad\n\n"
      "3\n00:00:01,500 --> 00:00:03,000\noverlap\n\n"
      "4\n00:00:04,000 --> 00:00:05,000\ntruncated";
  auto r = parse_srt(text);
  REQUIRE(r.cues.size() == 3);
  REQUIRE(r.cues[0].start_ms == 1000);
  REQUIRE(r.cues[0].raw_start == "00:00:01,000");
  REQUIRE(r.cues[0].byte_offset == 0);
  REQUIRE(r.cues.back().end_ms == 5000);  // no trailing newline still parsed
  bool saw_invalid = false, saw_overlap = false;
  for (const auto& a : r.anomalies) {
    saw_invalid = saw_invalid || a.message == "invalid_cue_timing";
    saw_overlap = saw_overlap || a.message == "overlapping_cues";
  }
  REQUIRE(saw_invalid);
  REQUIRE(saw_overlap);
}

TEST_CASE("webvtt parser handles identifiers and metadata", "[caption]") {
  std::string text =
      "WEBVTT\n\nNOTE a comment\nspans lines\n\n"
      "cue-id-1\n00:01.000 --> 00:02.500 align:center\nhi\n\n"
      "00:03.000 --> 00:04.000\nbye\n";
  auto r = parse_captions(text);
  REQUIRE(r.format == CaptionFormat::WebVtt);
  REQUIRE(r.cues.size() == 2);
  REQUIRE(r.cues[0].start_ms == 1000);
  REQUIRE(r.cues[0].end_ms == 2500);
}

TEST_CASE("bounded queue applies backpressure and cancel releases", "[pipeline]") {
  MemoryTracker mem(1 << 20);
  BoundedQueue<int> q(2, mem);
  CancelToken ct;
  REQUIRE(q.push(1, 100, ct));
  REQUIRE(q.push(2, 100, ct));
  std::atomic<bool> pushed{false};
  std::thread t([&] { pushed = q.push(3, 100, ct); });
  std::this_thread::sleep_for(std::chrono::milliseconds(50));
  REQUIRE_FALSE(pushed.load());  // blocked: backpressure
  REQUIRE(q.pop(ct).value() == 1);
  std::this_thread::sleep_for(std::chrono::milliseconds(50));
  REQUIRE(pushed.load());
  t.join();
  // Cancel with a full queue must release a blocked producer.
  REQUIRE(q.pop(ct).value() == 2);
  REQUIRE(q.push(4, 100, ct));  // queue now {3, 4}: full
  std::atomic<bool> pushed2{true};
  std::thread t2([&] { pushed2 = q.push(5, 100, ct); });
  std::this_thread::sleep_for(std::chrono::milliseconds(50));
  ct.cancel();
  t2.join();
  REQUIRE_FALSE(pushed2.load());  // cancelled push reports failure
}

TEST_CASE("memory tracker enforces ceiling and records peak", "[pipeline]") {
  MemoryTracker mem(1000);
  REQUIRE(mem.try_acquire(600));
  REQUIRE(mem.try_acquire(400));
  REQUIRE_FALSE(mem.try_acquire(1));
  REQUIRE(mem.peak() == 1000);
  mem.release(400);
  REQUIRE(mem.try_acquire(400));
}

TEST_CASE("sha256 matches known vectors", "[checkpoint]") {
  REQUIRE(Sha256::hex_of("abc") ==
          "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  REQUIRE(Sha256::hex_of("") ==
          "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
}

TEST_CASE("checkpoint round trip and refusal on changed input", "[checkpoint]") {
  std::string path = "test_ckpt.tmp";
  Checkpoint ck;
  ck.input_sha256 = Sha256::hex_of("input-a");
  ck.analysis_version = 1;
  ck.cursor = 512;
  ck.duplicate_packets = 3;
  DamageRange d;
  d.range = {{100, 1}, {200, 1}};
  d.kind = DamageKind::TransportLoss;
  d.severity = ErrorSeverity::Recoverable;
  d.note = "burst_loss";
  ck.completed_ranges.push_back(d);
  REQUIRE(save_checkpoint(path, ck));

  auto ok = load_checkpoint(path, ck.input_sha256, 1);
  REQUIRE(ok.status == LoadStatus::Ok);
  REQUIRE(ok.checkpoint.cursor == 512);
  REQUIRE(ok.checkpoint.completed_ranges.size() == 1);
  REQUIRE(ok.checkpoint.completed_ranges[0].note == "burst_loss");

  auto mismatch = load_checkpoint(path, Sha256::hex_of("input-b"), 1);
  REQUIRE(mismatch.status == LoadStatus::InputMismatch);
  auto ver = load_checkpoint(path, ck.input_sha256, 2);
  REQUIRE(ver.status == LoadStatus::VersionMismatch);
  std::remove(path.c_str());
}

static void write_packets(const std::string& path, int64_t count,
                          int64_t burst_at, int64_t burst_len) {
  std::ofstream out(path);
  for (int64_t s = 0; s < count; ++s) {
    if (burst_at >= 0 && s * 40 >= burst_at && s * 40 < burst_at + burst_len * 40)
      continue;
    out << "1,1," << s << "," << s * 40 << "," << s * 40 << ",1300\n";
  }
}

TEST_CASE("engine interrupt then resume equals uninterrupted run", "[engine]") {
  write_packets("test_pkts.csv", 1500, 20000, 10);
  EngineInputs in;
  in.packets_path = "test_pkts.csv";
  EngineConfig base;
  base.packet_window = 64;
  base.checkpoint_path = "test_engine.ckpt";

  auto full = run_analysis(in, base);
  REQUIRE(full.status == RunStatus::Ok);
  REQUIRE(full.burst_losses == 1);
  REQUIRE(full.damaged_ranges.size() == 1);
  REQUIRE(full.damaged_ranges[0].kind == DamageKind::TransportLoss);
  REQUIRE(rational_floor(full.damaged_ranges[0].range.start_ms) == 19960);

  EngineConfig first = base;
  first.interrupt_after_chunks = 3;
  auto inter = run_analysis(in, first);
  REQUIRE(inter.status == RunStatus::Interrupted);

  EngineConfig second = base;
  second.resume = true;
  auto resumed = run_analysis(in, second);
  REQUIRE(resumed.status == RunStatus::Ok);
  REQUIRE(resumed.resume_ms >= 0.0);
  REQUIRE(resumed.output_sha256 == full.output_sha256);

  // A changed input must refuse resume.
  write_packets("test_pkts.csv", 1500, 30000, 10);
  auto refused = run_analysis(in, second);
  REQUIRE(refused.status == RunStatus::CheckpointMismatch);

  std::remove("test_pkts.csv");
  std::remove("test_engine.ckpt");
}
