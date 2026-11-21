#include "relay/frame_decoder.hpp"
#include "relay/frame_record.hpp"

#include <cstdio>
#include <cstring>
#include <filesystem>
#include <string>
#include <vector>

namespace fs = std::filesystem;
using namespace relay;

namespace {

constexpr TimeBase kTb{1, 90000};

struct Gen {
  std::string dir;
  uint64_t total_records = 0;

  FrameRecord make(uint64_t seq, int64_t clock, bool keyframe) {
    FrameRecord r;
    r.frame_seq = seq;
    r.node_clock = clock;
    r.time_base = kTb;
    r.keyframe = keyframe;
    r.payload = encode_test_pattern_png(64, 64, seq);
    return r;
  }

  // VFR interval in 90kHz ticks: ~24-60fps, never a fixed step.
  static int64_t interval(uint64_t i) {
    return 1500 + static_cast<int64_t>((i * 7919) % 3000);
  }
};

// Main acceptance input: VFR, keyframes, one clock wrap, duplicates,
// a timeline gap and two checksum-corrupt frames.
void gen_mixed(Gen& g) {
  const uint64_t frames = 2400;
  const int64_t modulus = int64_t{1} << 33;
  FrameContainerWriter w(g.dir + "/mixed.frames");
  w.write_header(frames + 3);
  int64_t clock = modulus - 5'000'000; // wrap lands mid-stream
  uint64_t written = 0, corrupt = 0, dups = 0;
  FrameRecord dup_src;
  bool have_dup_src = false;
  for (uint64_t i = 0; i < frames; ++i) {
    if (i >= 1000 && i < 1005) { // 5 missing seqs -> one timeline gap
      clock += Gen::interval(i);
      continue;
    }
    FrameRecord r = g.make(i, clock % modulus, i % 30 == 0);
    if (i == 700 || i == 1700) { // corrupt checksum, payload intact
      r.checksum = record_checksum(r);
      r.payload[r.payload.size() / 2] ^= 0xFF;
      ++corrupt;
    }
    w.write_record(r);
    ++written;
    if (i == 300 || i == 900 || i == 1500) { // duplicate deliveries
      dup_src = r;
      have_dup_src = true;
      w.write_record(dup_src);
      ++written;
      ++dups;
    }
    clock += Gen::interval(i);
  }
  (void)have_dup_src;
  w.write_trailer();
  g.total_records += written;
  std::printf("fixture=mixed.frames records=%llu corrupt=%llu duplicates=%llu "
              "gaps=1 wraps=1\n",
              (unsigned long long)written, (unsigned long long)corrupt,
              (unsigned long long)dups);
}

void gen_wrap(Gen& g) {
  FrameContainerWriter w(g.dir + "/wrap.frames");
  const int64_t modulus = int64_t{1} << 33; // matches relay_check config
  const int64_t interval = modulus / 67;    // wraps every ~67 frames
  const uint64_t frames = 200;
  w.write_header(frames);
  int64_t clock = modulus - 20'000;
  for (uint64_t i = 0; i < frames; ++i) {
    w.write_record(g.make(i, clock % modulus, i % 30 == 0));
    clock += interval;
  }
  w.write_trailer();
  std::printf("fixture=wrap.frames records=%llu wraps_expected=3\n",
              (unsigned long long)frames);
}

void gen_drift(Gen& g) {
  FrameContainerWriter w(g.dir + "/drift.frames");
  const uint64_t frames = 120;
  w.write_header(frames);
  int64_t clock = 1'000'000;
  for (uint64_t i = 0; i < frames; ++i) {
    if (i == 60) clock += 90'000LL * 3600; // +1h jump: extreme drift
    w.write_record(g.make(i, clock, i % 30 == 0));
    clock += Gen::interval(i);
  }
  w.write_trailer();
  std::printf("fixture=drift.frames records=%llu drift_events=1\n",
              (unsigned long long)frames);
}

void gen_vfr_gap(Gen& g) {
  FrameContainerWriter w(g.dir + "/vfr_gap.frames");
  const uint64_t frames = 300;
  w.write_header(frames);
  int64_t clock = 0;
  uint64_t gaps = 0;
  for (uint64_t i = 0; i < frames; ++i) {
    if (i == 80 || i == 81 || i == 200) { // scattered missing frames
      clock += Gen::interval(i);
      ++gaps;
      continue;
    }
    w.write_record(g.make(i, clock, i % 25 == 0));
    clock += Gen::interval(i);
  }
  w.write_trailer();
  std::printf("fixture=vfr_gap.frames records=%llu missing=%llu\n",
              (unsigned long long)(frames - gaps), (unsigned long long)gaps);
}

void gen_corrupt(Gen& g) {
  FrameContainerWriter w(g.dir + "/corrupt.frames");
  const uint64_t frames = 100;
  w.write_header(frames);
  int64_t clock = 0;
  uint64_t bad_sum = 0, bad_payload = 0;
  for (uint64_t i = 0; i < frames; ++i) {
    FrameRecord r = g.make(i, clock, i % 20 == 0);
    if (i == 10 || i == 50) { // checksum mismatch
      r.checksum = record_checksum(r);
      r.payload[0] ^= 0xFF;
      ++bad_sum;
    } else if (i == 70) { // valid checksum, undecodable payload
      r.payload.assign(64, 0xAB);
      r.checksum = record_checksum(r);
      ++bad_payload;
    }
    w.write_record(r);
    clock += Gen::interval(i);
  }
  w.write_trailer();
  std::printf("fixture=corrupt.frames records=%llu bad_checksum=%llu "
              "bad_payload=%llu\n",
              (unsigned long long)frames, (unsigned long long)bad_sum,
              (unsigned long long)bad_payload);
}

void gen_truncated(Gen& g) {
  const std::string path = g.dir + "/truncated.frames";
  {
    FrameContainerWriter w(path);
    w.write_header(150);
    int64_t clock = 0;
    for (uint64_t i = 0; i < 150; ++i) {
      w.write_record(g.make(i, clock, i % 30 == 0));
      clock += Gen::interval(i);
    }
    w.write_trailer();
  }
  // Cut the file mid-record: unrecoverable container structure error.
  auto size = fs::file_size(path);
  fs::resize_file(path, size - 100);
  std::printf("fixture=truncated.frames records=150 truncated_tail=1\n");
}

void gen_bad_magic(Gen& g) {
  const std::string path = g.dir + "/bad_magic.frames";
  std::FILE* f = std::fopen(path.c_str(), "wb");
  std::fwrite("NOTARELAYCONTAINER...", 1, 21, f);
  std::fclose(f);
  std::printf("fixture=bad_magic.frames structural_error=bad_magic\n");
}

void gen_unknown_tb(Gen& g) {
  FrameContainerWriter w(g.dir + "/unknown_tb.frames");
  const uint64_t frames = 60;
  w.write_header(frames);
  int64_t clock = 0;
  for (uint64_t i = 0; i < frames; ++i) {
    FrameRecord r = g.make(i, clock, i % 30 == 0);
    if (i == 30) { // time base outside the accepted set
      r.time_base = {7, 13};
      r.checksum = record_checksum(r);
    }
    w.write_record(r);
    clock += Gen::interval(i);
  }
  w.write_trailer();
  std::printf("fixture=unknown_tb.frames records=%llu unknown_time_base=1\n",
              (unsigned long long)frames);
}

} // namespace

int main(int argc, char** argv) {
  std::string out;
  for (int i = 1; i + 1 < argc; ++i)
    if (std::strcmp(argv[i], "--out") == 0) out = argv[i + 1];
  if (out.empty()) {
    std::fprintf(stderr, "usage: generate_fixtures --out <dir>\n");
    return 2;
  }
  std::error_code ec;
  fs::create_directories(out, ec);
  if (ec) {
    std::fprintf(stderr, "cannot create %s: %s\n", out.c_str(),
                 ec.message().c_str());
    return 4;
  }
  Gen g{out, 0};
  gen_mixed(g);
  gen_wrap(g);
  gen_drift(g);
  gen_vfr_gap(g);
  gen_corrupt(g);
  gen_truncated(g);
  gen_bad_magic(g);
  gen_unknown_tb(g);
  std::printf("fixtures_done dir=%s\n", out.c_str());
  return 0;
}
