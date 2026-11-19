#include "sample_generator.hpp"

#include <algorithm>
#include <fstream>
#include <vector>

#include "byte_io.hpp"
#include "frame_transform.hpp"
#include "frame_pattern.hpp"
#include "packet_writer.hpp"
#include "rng.hpp"

namespace mfs {
namespace {

constexpr uint64_t kSimulatedStartUs = 5000000;
constexpr uint64_t kClockPeriod = uint64_t{1} << 32;

void set_arrival(Packet& packet, int64_t arrival_us) {
  std::vector<uint8_t> value;
  put_u64(value, static_cast<uint64_t>(arrival_us));
  for (MetadataField& field : packet.header.metadata) {
    if (field.id == kFieldArrivalUs) {
      field.value = std::move(value);
      return;
    }
  }
  packet.header.metadata.push_back(
      MetadataField{kFieldArrivalUs, FieldKind::Optional, std::move(value)});
}

enum class ClockDefect : uint8_t { None, Missing, Duplicate, Backward };

}  // namespace

GenerationStats generate_sample(const GeneratorOptions& options,
                                std::vector<Packet>& packets) {
  GenerationStats stats;
  packets.clear();
  const uint32_t frames = options.frames;
  if (frames == 0) {
    return stats;
  }

  FrameTransform transform;
  Rng rng(options.seed);
  const uint32_t width = std::max(options.width, 2u);
  const uint32_t height = std::max(options.height, 2u);
  const uint32_t frame_us = 1000000u / std::max(options.frame_rate, 1u);
  const uint32_t key_every = std::max(options.keyframe_interval, 1u);

  // Pre-roll decisions: which frames are lost, and which carry clock defects.
  std::vector<bool> dropped(frames, false);
  std::vector<ClockDefect> defects(frames, ClockDefect::None);
  if (options.loss_events > 0 && options.loss_burst > 0) {
    for (uint32_t event = 0; event < options.loss_events; ++event) {
      const uint32_t start = rng.below(frames);
      for (uint32_t k = 0; k < options.loss_burst; ++k) {
        dropped[(start + k) % frames] = true;
      }
    }
  }
  // Guarantee one of each clock defect when the budget allows, then scatter the
  // rest randomly. Guaranteed defects get frames of their own so the fixture
  // always exercises every repair path regardless of random draws.
  const uint32_t guaranteed = std::min<uint32_t>(options.timestamp_defects, 3);
  const uint32_t third = std::max<uint32_t>((frames - 8) / 3, 1);
  std::vector<bool> reserved(frames, false);
  for (uint32_t k = 0; k < guaranteed && frames > 8; ++k) {
    const uint32_t seq =
        4 + k * third + rng.below(std::min<uint32_t>(third, frames - 4));
    const uint32_t slot = std::min(seq, frames - 1);
    reserved[slot] = true;
    defects[slot] = static_cast<ClockDefect>(k + 1);
  }
  for (uint32_t k = guaranteed; k < options.timestamp_defects && frames > 8; ++k) {
    const uint32_t seq = 4 + rng.below(frames - 8);
    if (reserved[seq]) {
      continue;
    }
    switch (1 + rng.below(3)) {
      case 1:
        defects[seq] = ClockDefect::Missing;
        break;
      case 2:
        defects[seq] = ClockDefect::Duplicate;
        break;
      default:
        defects[seq] = ClockDefect::Backward;
        break;
    }
  }

  // The capture clock is a 32-bit microsecond counter seeded so it rolls over
  // once near the middle of the clip. The half-interval offset keeps any frame
  // from landing exactly on zero, which is reserved for "no timestamp".
  const uint32_t wrap_frame = frames / 2;
  const uint64_t clock_base = kClockPeriod -
                              static_cast<uint64_t>(wrap_frame) * frame_us -
                              frame_us / 2;

  std::vector<Packet> built;
  built.reserve(frames);
  for (uint32_t i = 0; i < frames; ++i) {
    if (dropped[i]) {
      ++stats.dropped;
      continue;
    }
    const bool keyframe = (i % key_every) == 0;
    Packet packet;
    packet.header.frame_seq = i;
    packet.header.capture_hz = 1000000;
    packet.header.width = width;
    packet.header.height = height;
    packet.header.codec = CodecId::Png;
    if (keyframe) {
      packet.header.meta_flags |= kFlagKeyframe;
    }

    const uint64_t absolute = clock_base + static_cast<uint64_t>(i) * frame_us;
    uint32_t clock = static_cast<uint32_t>(absolute);  // wraps at 2^32
    switch (defects[i]) {
      case ClockDefect::None:
        break;
      case ClockDefect::Missing:
        clock = 0;
        packet.header.meta_flags |= kFlagClockMissing;
        ++stats.clock_defects;
        break;
      case ClockDefect::Duplicate:
        // Copy the most recent *valid* clock so the defect is a genuine
        // duplicate, not an accidental second "missing" marker.
        for (auto it = built.rbegin(); it != built.rend(); ++it) {
          if (it->header.capture_clock != 0) {
            clock = it->header.capture_clock;
            ++stats.clock_defects;
            break;
          }
        }
        break;
      case ClockDefect::Backward:
        clock = static_cast<uint32_t>(clock - 3 * frame_us);
        ++stats.clock_defects;
        break;
    }
    packet.header.capture_clock = clock;

    const std::vector<uint8_t> image =
        render_pattern(width, height, i, keyframe);
    if (!transform.encode_png(image, width, height, packet.payload)) {
      continue;
    }
    built.push_back(std::move(packet));
  }

  // Delivery order: each packet is delayed by a random number of slots
  // inside the configured window, then sorted by delivery slot. A stable
  // sort keeps original order for equal slots, and the displacement of any
  // packet is bounded by the window depth (a swap chain is not bounded).
  struct Scheduled {
    uint64_t slot;
    size_t index;
  };
  // The maximum displacement a packet may experience is bounded by the
  // reorder window. A loss burst already consumes part of that budget (the
  // buffer must jump over the missing run before delayed packets arrive), so
  // the shuffle depth is reduced by the burst length. With window 16 and
  // burst 7 a packet is displaced by at most 7 + 8 = 15 slots, which the
  // receiver can still absorb.
  const uint32_t window = std::max<uint32_t>(options.reorder_window, 2);
  const uint32_t burst = std::min(options.loss_burst, window - 1);
  const uint32_t depth = std::max<uint32_t>(window - burst - 1, 1);
  std::vector<Scheduled> scheduled(built.size());
  for (size_t i = 0; i < built.size(); ++i) {
    // The first window packets stay in order so the receiver can establish its
    // sequence baseline without treating the head as late.
    const uint32_t delay = i < window ? 0 : rng.below(depth);
    scheduled[i] = Scheduled{static_cast<uint64_t>(i) + delay, i};
    if (delay > 0) {
      ++stats.reordered;
    }
  }
  std::stable_sort(scheduled.begin(), scheduled.end(),
                   [](const Scheduled& a, const Scheduled& b) {
                     return a.slot < b.slot;
                   });

  int64_t arrival_us = static_cast<int64_t>(kSimulatedStartUs);
  uint32_t gap_index = 0;
  for (const Scheduled& entry : scheduled) {
    const size_t index = entry.index;
    const uint32_t copies = rng.next_double() < options.duplicate_rate ? 2 : 1;
    arrival_us += static_cast<int64_t>(frame_us);
    if (!options.arrival_gaps.empty()) {
      const uint32_t gap =
          options.arrival_gaps[gap_index % options.arrival_gaps.size()];
      ++gap_index;
      arrival_us += static_cast<int64_t>(gap) * frame_us;
    }
    for (uint32_t copy = 0; copy < copies; ++copy) {
      Packet packet = built[index];
      set_arrival(packet, arrival_us);
      packets.push_back(std::move(packet));
    }
    if (copies > 1) {
      ++stats.duplicates;
    }
  }

  stats.frames = frames;
  stats.packets = static_cast<uint32_t>(packets.size());
  return stats;
}

std::vector<uint8_t> encode_container(const std::vector<Packet>& packets,
                                      uint64_t stream_id) {
  FileHeader header;
  header.stream_id = stream_id;
  header.nominal_hz = 1000000;
  std::vector<uint8_t> bytes = encode_file_header(header);
  size_t estimate = bytes.size();
  for (const Packet& packet : packets) {
    estimate += 8 + 64 + packet.payload.size();
  }
  bytes.reserve(estimate);
  for (const Packet& packet : packets) {
    const auto record = encode_record(packet);
    bytes.insert(bytes.end(), record.begin(), record.end());
  }
  return bytes;
}

void write_container(const std::string& path,
                     const std::vector<Packet>& packets, uint64_t stream_id) {
  const std::vector<uint8_t> bytes = encode_container(packets, stream_id);
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) {
    throw MediaError(ErrorType::LimitExceeded,
                     "cannot open output file: " + path);
  }
  out.write(reinterpret_cast<const char*>(bytes.data()),
            static_cast<std::streamsize>(bytes.size()));
  if (!out) {
    throw MediaError(ErrorType::LimitExceeded,
                     "cannot write output file: " + path);
  }
}

}  // namespace mfs
