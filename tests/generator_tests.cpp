#include <catch2/catch_test_macros.hpp>

#include <vector>

#include "packet_reader.hpp"
#include "rng.hpp"
#include "sample_generator.hpp"

using namespace mfs;

namespace {

GeneratorOptions small_options() {
  GeneratorOptions options;
  options.seed = 7;
  options.frames = 60;
  options.width = 64;
  options.height = 36;
  options.keyframe_interval = 10;
  options.loss_events = 2;
  options.loss_burst = 3;
  options.duplicate_rate = 0.1;
  options.reorder_rate = 0.2;
  options.timestamp_defects = 4;
  return options;
}

}  // namespace

TEST_CASE("the same seed produces identical bytes", "[generator]") {
  const auto options = small_options();
  std::vector<Packet> first;
  std::vector<Packet> second;
  generate_sample(options, first);
  generate_sample(options, second);
  REQUIRE(first.size() == second.size());
  CHECK(encode_container(first, 7) == encode_container(second, 7));
}

TEST_CASE("a different seed changes the delivery", "[generator]") {
  auto options = small_options();
  std::vector<Packet> first;
  std::vector<Packet> second;
  generate_sample(options, first);
  options.seed = 8;
  generate_sample(options, second);
  CHECK(encode_container(first, 7) != encode_container(second, 7));
}

TEST_CASE("generated clips parse back completely", "[generator]") {
  std::vector<Packet> packets;
  const GenerationStats stats = generate_sample(small_options(), packets);
  CHECK(stats.frames == 60);
  CHECK(stats.packets == packets.size());
  CHECK(stats.dropped > 0);
  CHECK(stats.duplicates > 0);

  const auto bytes = encode_container(packets, 9);
  PacketReader reader(bytes);
  size_t count = 0;
  Packet packet;
  while (reader.next(packet)) {
    ++count;
  }
  CHECK(count == packets.size());
  CHECK(reader.errors().empty());
  CHECK(reader.header().stream_id == 9);
}

TEST_CASE("lost frames leave sequence holes and duplicates are exact copies",
          "[generator]") {
  std::vector<Packet> packets;
  generate_sample(small_options(), packets);
  int duplicates = 0;
  for (size_t i = 1; i < packets.size(); ++i) {
    if (packets[i].header.frame_seq == packets[i - 1].header.frame_seq &&
        packets[i].payload == packets[i - 1].payload) {
      ++duplicates;
    }
  }
  CHECK(duplicates > 0);
}

TEST_CASE("the generator injects every supported clock defect",
          "[generator]") {
  GeneratorOptions options = small_options();
  options.frames = 200;
  options.timestamp_defects = 30;
  options.loss_events = 0;
  std::vector<Packet> packets;
  generate_sample(options, packets);
  bool missing = false;
  bool frozen = false;
  bool backward = false;
  for (size_t i = 1; i < packets.size(); ++i) {
    const uint32_t previous = packets[i - 1].header.capture_clock;
    const uint32_t current = packets[i].header.capture_clock;
    missing = missing || current == 0;
    frozen = frozen || (current == previous && current != 0);
    backward = backward || current < previous;
  }
  CHECK(missing);
  CHECK(frozen);
  CHECK(backward);
}

TEST_CASE("zero-frame generation is empty", "[generator]") {
  GeneratorOptions options;
  options.frames = 0;
  std::vector<Packet> packets;
  const auto stats = generate_sample(options, packets);
  CHECK(stats.packets == 0);
  CHECK(packets.empty());
}

TEST_CASE("rng is deterministic and bounded", "[rng]") {
  Rng a(123);
  Rng b(123);
  for (int i = 0; i < 100; ++i) {
    CHECK(a.next_u64() == b.next_u64());
  }
  for (int i = 0; i < 1000; ++i) {
    CHECK(a.below(7) < 7);
    const double d = a.next_double();
    CHECK(d >= 0.0);
    CHECK(d < 1.0);
  }
}
