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
  // Unrecognizable bytes yield no frames and a corruption diagnostic.
  std::vector<uint8_t> junk(2048, 0x00);
  for (size_t i = 0; i < junk.size(); ++i) junk[i] = static_cast<uint8_t>(i * 7);
  std::vector<ParsedFrame> frames;
  Diagnostic d = parse_elementary_stream(junk, CodecType::H264, {}, &frames);
  REQUIRE(!d.ok());
  REQUIRE(d.status == Status::MediaCorrupt);
  REQUIRE(d.offset >= 0);
  REQUIRE(frames.empty());

  // A dangling start code with no following NAL unit is also corrupt.
  std::vector<uint8_t> dangling = {0x00, 0x00, 0x00, 0x01, 0x67};
  std::vector<ParsedFrame> f2;
  Diagnostic d2 = parse_elementary_stream(dangling, CodecType::H264, {}, &f2);
  REQUIRE(!d2.ok());
  REQUIRE(d2.status == Status::MediaCorrupt);
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

#include "media_archive/audio_sync.h"

TEST_CASE("audio: archive round-trip preserves fragments", "[audio]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_audio_rt";
  fs::create_directories(dir);
  const fs::path pcm = dir / "audio.pcm";

  AudioArchive a;
  a.sample_rate = 24000;
  a.channels = 1;
  for (int f = 0; f < 3; ++f) {
    PcmFragment frag;
    frag.start_pts_ticks = f * kTimebaseHz / 10;  // 100 ms spacing
    frag.samples_s16.resize(2400);                 // 100 ms at 24 kHz
    for (size_t i = 0; i < frag.samples_s16.size(); ++i)
      frag.samples_s16[i] = static_cast<int16_t>((i * 37 + f) & 0xFFFF);
    a.fragments.push_back(std::move(frag));
  }
  REQUIRE(write_audio_archive(pcm.string(), a).ok());
  AudioArchive b;
  REQUIRE(read_audio_archive(pcm.string(), &b).ok());
  REQUIRE(b.sample_rate == 24000);
  REQUIRE(b.fragments.size() == 3);
  REQUIRE(b.fragments[2].samples_s16.size() == 2400);
  REQUIRE(b.fragments[1].start_pts_ticks == kTimebaseHz / 10);
}

TEST_CASE("audio: resample to 48k and align on shared clock", "[audio]") {
  AudioArchive a;
  a.sample_rate = 24000;
  a.channels = 1;
  PcmFragment frag;
  frag.start_pts_ticks = 0;
  frag.samples_s16.resize(2400);  // 100 ms of audio
  a.fragments.push_back(frag);
  PcmFragment late;
  late.start_pts_ticks = kTimebaseHz / 5;  // starts at 200 ms: 50 ms silence
  late.samples_s16.resize(2400);
  a.fragments.push_back(late);

  const uint64_t timeline = kTimebaseHz / 2;  // 500 ms
  AudioSyncResult r = align_audio(a, kCommonAudioRate, timeline);
  REQUIRE(r.fragments.size() == 2);
  // 2400 samples @24 kHz -> 4800 samples @48 kHz per fragment.
  REQUIRE(r.fragments[0].samples.size() == Catch::Approx(4800).margin(2));
  // Second fragment carries ~2400 silence samples (50 ms) before audio.
  REQUIRE(r.fragments[1].samples.size() >= 2400 + 4700);
  REQUIRE(r.sample_rate == 48000);
  REQUIRE(r.expected_samples == 24000);
  REQUIRE(r.max_sync_error_ms >= 0.0);
}

TEST_CASE("audio: truncated archive reports corrupt status", "[audio]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_audio_bad";
  fs::create_directories(dir);
  const fs::path pcm = dir / "bad.pcm";
  AudioArchive a;
  a.sample_rate = 48000;
  a.channels = 1;
  a.fragments.push_back({{}, std::vector<int16_t>(100, 1000)});
  REQUIRE(write_audio_archive(pcm.string(), a).ok());
  // Chop the last 40 bytes off (inside the sample payload).
  auto full = fs::file_size(pcm);
  std::vector<char> bytes(static_cast<size_t>(full) - 40);
  {
    std::ifstream in(pcm.string(), std::ios::binary);
    in.read(bytes.data(), static_cast<std::streamsize>(bytes.size()));
  }
  {
    std::ofstream out(pcm.string(), std::ios::binary | std::ios::trunc);
    out.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
  }
  AudioArchive b;
  Diagnostic d = read_audio_archive(pcm.string(), &b);
  REQUIRE(!d.ok());
  REQUIRE(d.status == Status::MediaCorrupt);
  REQUIRE(d.offset >= 0);
}

#include "media_archive/keyframe_index.h"

namespace {
std::vector<ParsedFrame> make_frames() {
  std::vector<ParsedFrame> frames;
  for (uint32_t i = 0; i < 9; ++i) {
    ParsedFrame f;
    f.source_offset = i * 100;
    f.size_bytes = 100;
    f.keyframe = (i % 3 == 0);
    f.frame_type = f.keyframe ? 1 : 2;  // AV_PICTURE_TYPE_I / P
    frames.push_back(f);
  }
  return frames;
}
std::vector<RepairedPacket> make_repaired() {
  std::vector<RepairedPacket> out;
  for (uint32_t i = 0; i < 9; ++i) {
    RepairedPacket rp;
    rp.packet.seq = i;
    rp.packet.source_offset = static_cast<int64_t>(i * 100);
    rp.repaired_pts = i * 3000;
    out.push_back(rp);
  }
  return out;
}
}  // namespace

TEST_CASE("index: only keyframes are indexed with offsets and crc",
          "[index]") {
  auto frames = make_frames();
  auto repaired = make_repaired();
  std::vector<uint8_t> stream(900);
  for (size_t i = 0; i < stream.size(); ++i) stream[i] = uint8_t(i * 13);
  auto entries = build_keyframe_index(frames, repaired, stream, 900);
  REQUIRE(entries.size() == 3);
  REQUIRE(entries[0].source_offset == 0);
  REQUIRE(entries[1].source_offset == 300);
  REQUIRE(entries[2].source_offset == 600);
  REQUIRE(entries[0].checksum != 0);
  const uint32_t expected = crc32_ieee(stream.data(), 100);
  REQUIRE(entries[0].checksum == expected);
}

TEST_CASE("index: seek lands at or before request with measured error",
          "[index]") {
  auto frames = make_frames();
  auto repaired = make_repaired();
  std::vector<uint8_t> stream(900, 0x55);
  auto entries = build_keyframe_index(frames, repaired, stream, 900);
  // Keyframes are frames 0,3,6 -> PTS 0,9000,18000 at offsets 0,300,600.
  SeekCheck c = verify_seek(entries, 4 * 3000);  // 12000, between 9k and 18k
  REQUIRE(c.landed_pts == 9000);
  REQUIRE(c.source_offset == 300);
  REQUIRE(c.error_ms == Catch::Approx(ticks_to_ms(3000)).margin(0.01));
  SeekCheck early = verify_seek(entries, 100);
  REQUIRE(early.landed_pts == 0);
  SeekCheck exact = verify_seek(entries, 9000);
  REQUIRE(exact.landed_pts == 9000);
  REQUIRE(exact.error_ms == Catch::Approx(0.0).margin(0.001));
}

TEST_CASE("index: serialization round-trip and corruption detection",
          "[index]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_index";
  fs::create_directories(dir);
  const fs::path idx = dir / "keyframes.idx";
  auto frames = make_frames();
  auto repaired = make_repaired();
  std::vector<uint8_t> stream(900, 0x33);
  auto entries = build_keyframe_index(frames, repaired, stream, 900);
  REQUIRE(write_keyframe_index(idx.string(), CodecType::H264, entries, 900)
              .ok());

  std::vector<KeyframeEntry> loaded;
  IndexFileHeader hdr{};
  REQUIRE(read_keyframe_index(idx.string(), CodecType::H264, &loaded, &hdr)
              .ok());
  REQUIRE(loaded.size() == entries.size());
  REQUIRE(loaded[2].source_offset == entries[2].source_offset);
  REQUIRE(hdr.stream_size == 900);

  // Flip a payload byte: the record checksum must fail.
  const auto size = fs::file_size(idx);
  {
    std::fstream f(idx.string(), std::ios::in | std::ios::out |
                                     std::ios::binary);
    f.seekp(static_cast<std::streamoff>(size) - 8);
    char flip = 0x7F;
    f.write(&flip, 1);
  }
  std::vector<KeyframeEntry> bad;
  Diagnostic d =
      read_keyframe_index(idx.string(), CodecType::H264, &bad, nullptr);
  REQUIRE(!d.ok());
  REQUIRE(d.status == Status::MediaCorrupt);
}

#include "media_archive/checkpoint_store.h"

namespace {
KeyframeEntry kf_entry(uint64_t pts, uint64_t off) {
  KeyframeEntry e;
  e.pts_ticks = pts;
  e.source_offset = off;
  e.size_bytes = 50;
  e.frame_type = 1;
  e.extended_seq = static_cast<uint32_t>(pts / 3000);
  e.checksum = crc32_ieee(reinterpret_cast<const uint8_t*>(&pts),
                          sizeof(pts));
  return e;
}
}  // namespace

TEST_CASE("checkpoint: blocks are written and recovered", "[checkpoint]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_cp_basic";
  fs::remove_all(dir);
  CheckpointConfig cfg;
  cfg.records_per_block = 10;
  {
    CheckpointStore store;
    REQUIRE(store.open(dir.string(), cfg).ok());
    for (uint64_t i = 0; i < 25; ++i)
      REQUIRE(store.append(kf_entry(i * 3000, i * 100)).ok());
    REQUIRE(store.flush().ok());
  }
  CheckpointOpenResult reopened;
  REQUIRE(open_checkpoint_dir(dir.string(), cfg, &reopened).ok());
  REQUIRE(reopened.recovered.size() == 25);
  REQUIRE(reopened.recovered.front().pts_ticks == 0);
  REQUIRE(reopened.recovered.back().pts_ticks == 24 * 3000);
  REQUIRE(reopened.last_valid_block == 2);
}

TEST_CASE("checkpoint: crash then resume keeps only durable blocks",
          "[checkpoint]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_cp_crash";
  fs::remove_all(dir);
  CheckpointConfig cfg;
  cfg.records_per_block = 10;
  {
    CheckpointStore store;
    REQUIRE(store.open(dir.string(), cfg).ok());
    for (uint64_t i = 0; i < 25; ++i) {
      Diagnostic d = store.append(kf_entry(i * 3000, i * 100),
                                  /*crash_after_appends=*/15);
      if (i >= 15) { REQUIRE(!d.ok()); break; }
      REQUIRE(d.ok());
    }
    // Destructor runs; flush must not resurrect uncommitted appends.
  }
  CheckpointOpenResult reopened;
  REQUIRE(open_checkpoint_dir(dir.string(), cfg, &reopened).ok());
  REQUIRE(reopened.resumed);
  // Only the first fully finalized block (10 records) survives.
  REQUIRE(reopened.recovered.size() == 10);
  REQUIRE(reopened.last_valid_block == 0);
}

TEST_CASE("checkpoint: corrupt tail block rolls back", "[checkpoint]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_cp_tail";
  fs::remove_all(dir);
  CheckpointConfig cfg;
  cfg.records_per_block = 10;
  {
    CheckpointStore store;
    REQUIRE(store.open(dir.string(), cfg).ok());
    for (uint64_t i = 0; i < 30; ++i)
      REQUIRE(store.append(kf_entry(i * 3000, i * 100)).ok());
    REQUIRE(store.flush().ok());
  }
  // Corrupt the last block (block 2) in place.
  fs::path last = dir / "block-000002.cp";
  REQUIRE(fs::exists(last));
  {
    std::fstream f(last.string(),
                   std::ios::in | std::ios::out | std::ios::binary);
    f.seekp(20);  // inside the record area
    char flip = 0x5A;
    f.write(&flip, 1);
  }
  CheckpointOpenResult reopened;
  REQUIRE(open_checkpoint_dir(dir.string(), cfg, &reopened).ok());
  REQUIRE(reopened.rolled_back_block == 2);
  REQUIRE(reopened.last_valid_block == 1);
  REQUIRE(reopened.recovered.size() == 20);
}

TEST_CASE("checkpoint: repeated runs are deterministic", "[checkpoint]") {
  namespace fs = std::filesystem;
  const fs::path a = fs::temp_directory_path() / "ma_cp_det_a";
  const fs::path b = fs::temp_directory_path() / "ma_cp_det_b";
  fs::remove_all(a);
  fs::remove_all(b);
  CheckpointConfig cfg;
  cfg.records_per_block = 7;
  auto build = [&](const fs::path& d) {
    CheckpointStore store;
    REQUIRE(store.open(d.string(), cfg).ok());
    for (uint64_t i = 0; i < 18; ++i)
      REQUIRE(store.append(kf_entry(i * 3000, i * 100)).ok());
    REQUIRE(store.flush().ok());
  };
  build(a);
  build(b);
  CheckpointOpenResult ra, rb;
  REQUIRE(open_checkpoint_dir(a.string(), cfg, &ra).ok());
  REQUIRE(open_checkpoint_dir(b.string(), cfg, &rb).ok());
  REQUIRE(ra.recovered.size() == rb.recovered.size());
  for (size_t i = 0; i < ra.recovered.size(); ++i) {
    REQUIRE(ra.recovered[i].pts_ticks == rb.recovered[i].pts_ticks);
    REQUIRE(ra.recovered[i].checksum == rb.recovered[i].checksum);
  }
}

TEST_CASE("checkpoint: resume after crash completes deterministically",
          "[checkpoint]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_cp_resume";
  const fs::path clean = fs::temp_directory_path() / "ma_cp_clean";
  fs::remove_all(dir);
  fs::remove_all(clean);
  CheckpointConfig cfg;
  cfg.records_per_block = 10;

  // Interrupted run: crash after 15 appends.
  {
    CheckpointStore store;
    REQUIRE(store.open(dir.string(), cfg).ok());
    for (uint64_t i = 0; i < 25; ++i) {
      Diagnostic d = store.append(kf_entry(i * 3000, i * 100), 15);
      if (!d.ok()) break;
    }
  }
  // Resume on a fresh store instance and finish the remaining entries.
  {
    CheckpointStore store;
    REQUIRE(store.open(dir.string(), cfg).ok());
    for (uint64_t i = 10; i < 25; ++i)
      REQUIRE(store.append(kf_entry(i * 3000, i * 100)).ok());
    REQUIRE(store.flush().ok());
  }
  // Clean reference run, never interrupted.
  {
    CheckpointStore store;
    REQUIRE(store.open(clean.string(), cfg).ok());
    for (uint64_t i = 0; i < 25; ++i)
      REQUIRE(store.append(kf_entry(i * 3000, i * 100)).ok());
    REQUIRE(store.flush().ok());
  }
  CheckpointOpenResult ra, rb;
  REQUIRE(open_checkpoint_dir(dir.string(), cfg, &ra).ok());
  REQUIRE(open_checkpoint_dir(clean.string(), cfg, &rb).ok());
  REQUIRE(ra.recovered.size() == 25);
  REQUIRE(ra.recovered.size() == rb.recovered.size());
  for (size_t i = 0; i < 25; ++i) {
    REQUIRE(ra.recovered[i].pts_ticks == rb.recovered[i].pts_ticks);
    REQUIRE(ra.recovered[i].source_offset == rb.recovered[i].source_offset);
    REQUIRE(ra.recovered[i].checksum == rb.recovered[i].checksum);
  }
}

#include "media_archive/sample_generator.h"

TEST_CASE("generator: all three codecs encode keyframes", "[generator]") {
  for (CodecType codec : {CodecType::H264, CodecType::H265, CodecType::AV1}) {
    SampleConfig cfg;
    cfg.seed = 42;
    cfg.codec = codec;
    cfg.frame_count = 45;
    cfg.gop = 15;
    std::vector<EncodedFrame> frames;
    Diagnostic d = encode_video(cfg, &frames);
    REQUIRE(d.ok());
    REQUIRE(frames.size() == 45);
    const auto keyframes =
        std::count_if(frames.begin(), frames.end(),
                      [](const EncodedFrame& f) { return f.keyframe; });
    REQUIRE(keyframes >= 3);  // GOP 15 over 45 frames
    REQUIRE(frames.front().keyframe);
    // The bytes must parse as the declared codec.
    std::vector<uint8_t> stream = build_elementary_stream(codec, frames);
    std::vector<ParsedFrame> parsed;
    REQUIRE(parse_elementary_stream(stream, codec, {}, &parsed).ok());
    REQUIRE(parsed.size() == 45);
  }
}

TEST_CASE("generator: full sample bundle is coherent on disk",
          "[generator]") {
  namespace fs = std::filesystem;
  const fs::path dir = fs::temp_directory_path() / "ma_bundle";
  fs::remove_all(dir);
  SampleConfig cfg;
  cfg.seed = 42;
  cfg.codec = CodecType::H264;
  cfg.frame_count = 120;
  cfg.gop = 30;
  GeneratedSample sample;
  REQUIRE(generate_sample(cfg, &sample).ok());
  REQUIRE(write_sample_bundle(dir.string(), sample).ok());

  REQUIRE(fs::exists(dir / "stream.h264"));
  REQUIRE(fs::exists(dir / "packets.csv"));
  REQUIRE(fs::exists(dir / "audio.pcm"));
  REQUIRE(fs::file_size(dir / "stream.h264") ==
          sample.received_video.size());
  REQUIRE(sample.trace.gap_packet_count > 0);

  // The received stream must parse, and present frames must match manifest.
  std::vector<ParsedFrame> parsed;
  REQUIRE(parse_elementary_stream(sample.received_video, CodecType::H264,
                                  {}, &parsed)
              .ok());
  std::vector<TransportPacket> manifest;
  REQUIRE(parse_packet_manifest((dir / "packets.csv").string(), &manifest)
              .ok());
  const auto present = std::count_if(
      manifest.begin(), manifest.end(),
      [](const TransportPacket& p) { return p.flag != PacketFlag::Gap; });
  REQUIRE(parsed.size() == static_cast<size_t>(present));

  // Audio archive round-trips and covers the timeline.
  AudioArchive audio;
  REQUIRE(read_audio_archive((dir / "audio.pcm").string(), &audio).ok());
  REQUIRE(audio.fragments.size() == cfg.frame_count / 6);
  REQUIRE(audio.sample_rate == cfg.audio_rate);
}
