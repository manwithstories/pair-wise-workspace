#include <catch2/catch_test_macros.hpp>

#include <vector>

#include "byte_io.hpp"
#include "reorder_buffer.hpp"

using namespace mfs;

namespace {

Packet make_packet(uint64_t seq, int64_t arrival_us = -1) {
  Packet packet;
  packet.header.frame_seq = seq;
  packet.header.capture_clock = static_cast<uint32_t>(seq + 1);
  packet.header.capture_hz = 1000000;
  packet.payload.assign(8, static_cast<uint8_t>(seq));
  packet.raw_offset = seq * 1000;
  if (arrival_us >= 0) {
    std::vector<uint8_t> value;
    put_u64(value, static_cast<uint64_t>(arrival_us));
    packet.header.metadata.push_back(
        MetadataField{kFieldArrivalUs, FieldKind::Optional, value});
  }
  return packet;
}

std::vector<uint64_t> seqs_of(const std::vector<Packet>& packets) {
  std::vector<uint64_t> out;
  for (const Packet& packet : packets) {
    out.push_back(packet.header.frame_seq);
  }
  return out;
}

std::vector<Packet> push_all(ReorderBuffer& buffer, uint64_t first,
                             uint64_t last) {
  std::vector<Packet> out;
  for (uint64_t seq = first; seq <= last; ++seq) {
    auto released = buffer.push(make_packet(seq));
    out.insert(out.end(), released.begin(), released.end());
  }
  return out;
}

}  // namespace

TEST_CASE("in-order packets pass straight through", "[reorder]") {
  ReorderBuffer buffer;
  const auto released = push_all(buffer, 0, 9);
  CHECK(seqs_of(released) == std::vector<uint64_t>{0, 1, 2, 3, 4, 5, 6, 7, 8, 9});
  CHECK(buffer.stats().duplicates == 0);
  CHECK(buffer.stats().out_of_window == 0);
  CHECK(buffer.gaps().empty());
  CHECK(buffer.buffered_count() == 0);
}

TEST_CASE("out-of-order packets are restored to sequence order", "[reorder]") {
  ReorderBuffer buffer;
  std::vector<Packet> released;
  for (uint64_t seq : {2ull, 0ull, 1ull, 4ull, 3ull}) {
    auto batch = buffer.push(make_packet(seq));
    released.insert(released.end(), batch.begin(), batch.end());
  }
  CHECK(seqs_of(released) == std::vector<uint64_t>{0, 1, 2, 3, 4});
}

TEST_CASE("duplicates are idempotent", "[reorder]") {
  ReorderBuffer buffer;
  std::vector<Packet> released;
  for (uint64_t seq : {0ull, 1ull, 1ull, 1ull, 2ull}) {
    auto batch = buffer.push(make_packet(seq));
    released.insert(released.end(), batch.begin(), batch.end());
  }
  CHECK(seqs_of(released) == std::vector<uint64_t>{0, 1, 2});
  CHECK(buffer.stats().duplicates == 2);
  CHECK(buffer.errors().empty());
  CHECK(buffer.gaps().empty());
}

TEST_CASE("bounded window releases past a hole and reports a gap",
          "[reorder]") {
  ReorderOptions options;
  options.base_window = 2;
  options.adaptive_window = false;
  ReorderBuffer buffer(options);

  std::vector<Packet> released;
  for (uint64_t seq : {0ull, 1ull, 3ull, 4ull, 5ull}) {
    auto batch = buffer.push(make_packet(seq));
    released.insert(released.end(), batch.begin(), batch.end());
  }
  // Sequence 2 never arrives: the window forces progress past it.
  CHECK(seqs_of(released) == std::vector<uint64_t>{0, 1, 3, 4, 5});
  REQUIRE(buffer.gaps().size() == 1);
  CHECK(buffer.gaps()[0].first_seq == 2);
  CHECK(buffer.gaps()[0].last_seq == 2);
}

TEST_CASE("packet arriving after its gap was abandoned is out of window",
          "[reorder]") {
  ReorderOptions options;
  options.base_window = 1;
  options.adaptive_window = false;
  ReorderBuffer buffer(options);

  buffer.push(make_packet(0));
  buffer.push(make_packet(2));  // window == 1: jumps over 1
  buffer.push(make_packet(1));  // too late
  REQUIRE(buffer.errors().size() == 1);
  CHECK(buffer.errors()[0].type == ErrorType::OutOfWindow);
  CHECK(buffer.errors()[0].offset == 1000);
  CHECK(buffer.stats().out_of_window == 1);
}

TEST_CASE("memory stays bounded under sustained loss", "[reorder]") {
  ReorderOptions options;
  options.base_window = 4;
  options.max_buffered_packets = 8;
  options.adaptive_window = false;
  ReorderBuffer buffer(options);

  for (uint64_t seq = 0; seq < 500; seq += 2) {
    buffer.push(make_packet(seq));
    CHECK(buffer.buffered_count() <= options.max_buffered_packets);
  }
  CHECK(buffer.stats().buffered_peak <= options.max_buffered_packets + 1);
  CHECK(!buffer.gaps().empty());
}

TEST_CASE("fighting jitter widens the adaptive window", "[reorder]") {
  ReorderOptions options;
  options.base_window = 4;
  options.max_window = 64;
  options.nominal_interval_us = 10000;
  ReorderBuffer buffer(options);

  buffer.push(make_packet(0, 0));
  buffer.push(make_packet(1, 10000));
  const uint32_t steady = buffer.effective_window();
  // A 150 ms stall: 15 nominal intervals behind, so a burst is expected.
  buffer.push(make_packet(2, 160000));
  CHECK(buffer.effective_window() > steady);
  CHECK(buffer.effective_window() <= options.max_window);
  CHECK(buffer.stats().adaptive_widenings >= 1);
}

TEST_CASE("flush releases remaining packets in sequence order", "[reorder]") {
  ReorderBuffer buffer;
  buffer.push(make_packet(0));
  buffer.push(make_packet(2));
  buffer.push(make_packet(3));
  const auto rest = buffer.flush();
  CHECK(seqs_of(rest) == std::vector<uint64_t>{2, 3});
  CHECK(buffer.buffered_count() == 0);
}
