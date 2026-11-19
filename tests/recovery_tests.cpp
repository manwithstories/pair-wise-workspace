#include <catch2/catch_test_macros.hpp>
#include <catch2/catch_approx.hpp>

#include <filesystem>
#include <fstream>
#include <vector>

#include "media_archive/packet_reader.h"
#include "media_archive/sequence_repair.h"
#include "media_archive/transport_trace.h"

using namespace media_archive;

namespace {
TransportPacket pkt(uint32_t seq, PacketFlag flag, uint32_t size = 100) {
  TransportPacket p;
  p.seq = seq;
  p.size_bytes = size;
  p.source_offset = flag == PacketFlag::Gap ? -1 : 0;
  p.flag = flag;
  return p;
}
}  // namespace

TEST_CASE("trace: deterministic and reproducible", "[trace]") {
  std::vector<uint32_t> sizes(120, 900);
  TraceConfig cfg;
  cfg.seed = 42;
  auto a = build_transport_trace(120, sizes, cfg);
  auto b = build_transport_trace(120, sizes, cfg);
  REQUIRE(a.packets.size() == b.packets.size());
  for (size_t i = 0; i < a.packets.size(); ++i) {
    REQUIRE(a.packets[i].seq == b.packets[i].seq);
    REQUIRE(a.packets[i].flag == b.packets[i].flag);
    REQUIRE(a.packets[i].recv_ts_ms == b.packets[i].recv_ts_ms);
  }
  REQUIRE(a.gap_packet_count > 0);
  REQUIRE(percentile(a.latencies_ms, 0.95) > 0.0);
}

TEST_CASE("sequence: in-order packets are unchanged", "[sequence]") {
  std::vector<TransportPacket> in{pkt(0, PacketFlag::Ok),
                                  pkt(1, PacketFlag::Ok),
                                  pkt(2, PacketFlag::Ok)};
  auto r = repair_sequence(in);
  REQUIRE(r.ordered.size() == 3);
  REQUIRE(r.duplicate_count == 0);
  REQUIRE(r.gaps.empty());
  REQUIRE(r.unrecovered_bytes == 0);
  REQUIRE(r.ordered[0].seq == 0);
}

TEST_CASE("sequence: duplicates are idempotent", "[sequence]") {
  std::vector<TransportPacket> in{pkt(0, PacketFlag::Ok),
                                  pkt(1, PacketFlag::Ok),
                                  pkt(1, PacketFlag::Duplicate),
                                  pkt(2, PacketFlag::Ok)};
  auto r = repair_sequence(in);
  REQUIRE(r.ordered.size() == 3);
  REQUIRE(r.duplicate_count == 1);
  // Running twice yields the same result.
  auto r2 = repair_sequence(in);
  REQUIRE(r2.ordered.size() == r.ordered.size());
  REQUIRE(r2.duplicate_count == r.duplicate_count);
}

TEST_CASE("sequence: reordering is repaired", "[sequence]") {
  std::vector<TransportPacket> in{pkt(0, PacketFlag::Ok),
                                  pkt(2, PacketFlag::Reordered),
                                  pkt(1, PacketFlag::Reordered),
                                  pkt(3, PacketFlag::Ok)};
  auto r = repair_sequence(in);
  REQUIRE(r.ordered.size() == 4);
  for (size_t i = 1; i < r.ordered.size(); ++i)
    REQUIRE(r.ordered[i].seq == r.ordered[i - 1].seq + 1);
}

TEST_CASE("sequence: burst loss becomes one gap interval", "[sequence]") {
  std::vector<TransportPacket> in{pkt(0, PacketFlag::Ok),
                                  pkt(1, PacketFlag::Gap, 111),
                                  pkt(2, PacketFlag::Gap, 222),
                                  pkt(3, PacketFlag::Ok)};
  auto r = repair_sequence(in);
  REQUIRE(r.ordered.size() == 2);
  REQUIRE(r.gaps.size() == 1);
  REQUIRE(r.gaps[0].extended_seq_first == 1);
  REQUIRE(r.gaps[0].extended_seq_last == 2);
  REQUIRE(r.gaps[0].missing_bytes == 333);
  REQUIRE(r.unrecovered_bytes == 333);
  REQUIRE(r.expected_unique == 4);
}

TEST_CASE("sequence: 16-bit wraparound differs from replay", "[sequence]") {
  // Walk up to the wrap boundary, then continue past it.
  std::vector<TransportPacket> in;
  for (uint32_t s = 65533; s != 2; s = static_cast<uint32_t>((s + 1) % 65536))
    in.push_back(pkt(s, PacketFlag::Ok));
  auto r = repair_sequence(in);
  REQUIRE(r.ordered.size() == 5);  // 65533,65534,65535,0,1
  REQUIRE(r.ordered.front().seq == 65533);
  REQUIRE(r.ordered.back().seq == 1);
  REQUIRE(r.replay_count == 0);

  // A genuinely old packet (far beyond the reorder window) is a replay.
  in.push_back(pkt(30000, PacketFlag::Duplicate));
  auto r2 = repair_sequence(in);
  REQUIRE(r2.ordered.size() == 5);
  REQUIRE(r2.replay_count == 1);
}

TEST_CASE("sequence: trace-level recovery rate stays above 90%", "[sequence]") {
  std::vector<uint32_t> sizes(300, 1200);
  TraceConfig cfg;
  cfg.seed = 42;
  auto trace = build_transport_trace(300, sizes, cfg);
  auto r = repair_sequence(trace.packets);
  const double rate =
      static_cast<double>(r.ordered.size()) /
      static_cast<double>(r.ordered.size() + trace.gap_packet_count);
  REQUIRE(rate >= Catch::Approx(0.90));
  REQUIRE(r.unrecovered_bytes > 0);
}

TEST_CASE("reader: manifest round-trips through CSV", "[reader]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_reader_test";
  fs::create_directories(dir);
  const fs::path csv = dir / "packets.csv";

  std::vector<TransportPacket> in;
  for (uint32_t i = 0; i < 10; ++i) {
    TransportPacket p;
    p.seq = i;
    p.pts_ticks = i * 3000;
    p.source_offset = i == 4 ? -1 : static_cast<int64_t>(i * 128);
    p.size_bytes = 128;
    p.flag = i == 4 ? PacketFlag::Gap : PacketFlag::Ok;
    p.send_ts_ms = i * 33.3;
    p.recv_ts_ms = i * 33.3 + 1.5;
    in.push_back(p);
  }
  REQUIRE(write_packet_manifest(csv.string(), in).ok());
  std::vector<TransportPacket> out;
  REQUIRE(parse_packet_manifest(csv.string(), &out).ok());
  REQUIRE(out.size() == in.size());
  REQUIRE(out[4].flag == PacketFlag::Gap);
  REQUIRE(out[4].source_offset == -1);
  REQUIRE(out[7].pts_ticks == 7 * 3000);
}

TEST_CASE("reader: input over the size limit is rejected", "[reader]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_reader_limit";
  fs::create_directories(dir);
  const fs::path f = dir / "big.bin";
  {
    std::ofstream os(f.string(), std::ios::binary);
    std::vector<char> buf(4096, 0xAB);
    os.write(buf.data(), static_cast<std::streamsize>(buf.size()));
  }
  ReaderLimits limits;
  limits.max_input_bytes = 1024;
  std::vector<uint8_t> out;
  Diagnostic d = read_file_bounded(f.string(), limits, &out);
  REQUIRE(!d.ok());
  REQUIRE(d.status == Status::UsageError);
}

TEST_CASE("reader: garbage stream reports failure with offset", "[reader]") {
  std::vector<uint8_t> junk(2048, 0x00);
  for (size_t i = 0; i < junk.size(); ++i) junk[i] = static_cast<uint8_t>(i * 7);
  std::vector<ParsedFrame> frames;
  Diagnostic d = parse_elementary_stream(junk, CodecType::H264, {}, &frames);
  REQUIRE(!d.ok());
  REQUIRE(d.status == Status::MediaCorrupt);
  REQUIRE(d.offset >= 0);
  REQUIRE(frames.empty());
}

#include "media_archive/timestamp_repair.h"

namespace {
TransportPacket tp(uint32_t seq, uint64_t pts) {
  TransportPacket p;
  p.seq = seq;
  p.pts_ticks = pts;
  p.size_bytes = 100;
  p.source_offset = seq * 100;
  p.flag = PacketFlag::Ok;
  return p;
}
}  // namespace

TEST_CASE("pts: 33-bit wraparound is unwrapped", "[pts]") {
  const uint64_t mod = 1ull << 33;
  const uint64_t step = 3000;
  std::vector<TransportPacket> in;
  // Place the last frames just before and after the wrap boundary.
  for (uint64_t i = 0; i < 8; ++i)
    in.push_back(tp(static_cast<uint32_t>(i),
                    (mod - 3 * step + i * step) % mod));
  TimestampResult r = repair_timestamps(in, {});
  REQUIRE(r.wrap_count == 1);
  for (size_t i = 1; i < r.packets.size(); ++i) {
    REQUIRE(r.packets[i].repaired_pts > r.packets[i - 1].repaired_pts);
    REQUIRE(r.packets[i].repaired_pts -
                r.packets[i - 1].repaired_pts ==
            Catch::Approx(step).margin(1));
  }
  REQUIRE(r.packets[3].pts_wrapped);
  REQUIRE_FALSE(r.packets[4].pts_wrapped);
}

TEST_CASE("pts: configured clock drift is removed", "[pts]") {
  std::vector<TransportPacket> in;
  const uint64_t step = 3000;
  const double ppm = 500.0;  // 0.05% fast
  for (uint64_t i = 0; i < 20; ++i) {
    uint64_t pts = static_cast<uint64_t>(
        std::llround(i * step * (1.0 + ppm / 1'000'000.0)));
    in.push_back(tp(static_cast<uint32_t>(i), pts));
  }
  TimestampConfig cfg;
  cfg.drift_ppm = ppm;
  TimestampResult r = repair_timestamps(in, {}, cfg);
  // After correction the tail delta must be back near the nominal step.
  const uint64_t tail =
      r.packets.back().repaired_pts - r.packets.front().repaired_pts;
  REQUIRE(tail == Catch::Approx(19 * step).margin(2));
}

TEST_CASE("pts: non-monotonic jump without a gap is corrected", "[pts]") {
  std::vector<TransportPacket> in;
  for (uint64_t i = 0; i < 6; ++i)
    in.push_back(tp(static_cast<uint32_t>(i), i * 3000));
  in[4].pts_ticks = in[3].pts_ticks - 500;  // backwards jump
  std::vector<uint32_t> gaps(6, 0);
  TimestampResult r = repair_timestamps(in, gaps);
  REQUIRE(r.corrected_jump_count >= 1);
  REQUIRE(r.uncertain_count == 0);
  REQUIRE(r.packets[4].repaired_pts > r.packets[3].repaired_pts);
}

TEST_CASE("pts: jump after a burst gap is marked uncertain", "[pts]") {
  std::vector<TransportPacket> in;
  for (uint64_t i = 0; i < 6; ++i)
    in.push_back(tp(static_cast<uint32_t>(i), i * 3000));
  in[4].pts_ticks = in[3].pts_ticks + 20 * 3000;  // unexplained large jump
  std::vector<uint32_t> gaps(6, 0);
  gaps[4] = 3;  // three missing packets precede this arrival
  TimestampResult r = repair_timestamps(in, gaps);
  REQUIRE(r.uncertain_count >= 1);
  REQUIRE(r.packets[4].pts_uncertain);
  // The observed value is preserved rather than silently rewritten.
  REQUIRE(r.packets[4].repaired_pts == in[4].pts_ticks);
}
