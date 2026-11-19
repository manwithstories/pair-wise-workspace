#include <catch2/catch_test_macros.hpp>
#include <catch2/catch_approx.hpp>

#include <vector>

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
