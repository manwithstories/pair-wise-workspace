#include <algorithm>
#include <cstring>

#include <catch2/catch_test_macros.hpp>
#include <catch2/generators/catch_generators.hpp>

#include "capture.hpp"
#include "common.hpp"
#include "rtp_ingest.hpp"
#include "sha256.hpp"

using namespace mb;

namespace {
RawRecord make_rtp(u16 track, u32 ssrc, u16 seq, u32 ts, bool key = false,
                   u8 ext = 0x00) {
  RawRecord r;
  r.type = RecordType::Rtp;
  r.track_id = track;
  RtpHeaderOnWire h{};
  h.vpxcc = u8(kRtpVersion << 6);
  h.mpt = 96;
  h.seq = static_cast<u16>(seq);
  h.timestamp = ts;
  h.ssrc = ssrc;
  std::vector<u8> body(sizeof(h) + 1, 0);
  std::memcpy(body.data(), &h, sizeof(h));
  body[sizeof(h)] = ext == 0 ? (key ? 0x01 : 0x00) : ext;
  r.body = std::move(body);
  return r;
}

TrackInfo video_track(u16 id, u32 ssrc) {
  TrackInfo t;
  t.track_id = id;
  t.ssrc = ssrc;
  t.kind = TrackKind::Video;
  t.clock_rate = 90000;
  t.name = "v" + std::to_string(id);
  return t;
}
}  // namespace

TEST_CASE("SHA-256 matches known empty and abc digests", "[sha256]") {
  CHECK(Sha256::hex(Sha256::hash({})) ==
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  std::vector<u8> abc = {'a', 'b', 'c'};
  CHECK(Sha256::hex(Sha256::hash(abc)) ==
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
}

TEST_CASE("RTP reorder restores swapped packets and measures depth",
          "[ingest]") {
  auto t = video_track(0, 111);
  RtpIngest ing({t});
  // Arrival order 0,1,3,2,4 -> reordered to 0..4.
  for (u16 s : {u16(0), u16(1), u16(3), u16(2), u16(4)})
    ing.push(make_rtp(0, 111, s, 1000u + u32(s) * 3000u));
  auto r = ing.finish(0);
  REQUIRE(r.packets.size() == 5);
  CHECK(std::is_sorted(r.packets.begin(), r.packets.end(),
                       [](const PacketRecord& a, const PacketRecord& b) {
                         return a.abs_seq < b.abs_seq;
                       }));
  CHECK(r.stats.at(111).max_reorder_depth >= 2);
  CHECK(r.stats.at(111).lost == 0);
}

TEST_CASE("RTP sequence wrap is handled modulo 65536", "[ingest]") {
  auto t = video_track(0, 222);
  RtpIngest ing({t});
  ing.push(make_rtp(0, 222, 65534, 100));
  ing.push(make_rtp(0, 222, 65535, 200));
  ing.push(make_rtp(0, 222, 0, 300));
  ing.push(make_rtp(0, 222, 1, 400));
  auto r = ing.finish(0);
  REQUIRE(r.packets.size() == 4);
  CHECK(r.packets.front().seq == 65534);
  CHECK(r.packets.back().seq == 1);
  CHECK(r.packets[2].abs_seq == 65536);
  CHECK(r.stats.at(222).lost == 0);
}

TEST_CASE("RTP gap past reorder window is unrecoverable", "[ingest]") {
  auto t = video_track(0, 333);
  RtpIngest ing({t});
  ing.push(make_rtp(0, 333, 0, 0));
  // Jump far beyond the 64-packet window, skipping sequences 1..200.
  ing.push(make_rtp(0, 333, 201, 603000));
  auto r = ing.finish(0);
  CHECK(r.stats.at(333).lost >= 200);
  bool has_gap = std::any_of(r.errors.begin(), r.errors.end(),
                             [](const ErrorRecord& e) {
                               return e.code == "rtp.gap" &&
                                      e.recovery ==
                                          Recoverability::Unrecoverable;
                             });
  CHECK(has_gap);
}

TEST_CASE("Duplicate and late packets are counted, not re-emitted",
          "[ingest]") {
  auto t = video_track(0, 444);
  RtpIngest ing({t});
  ing.push(make_rtp(0, 444, 10, 0));
  ing.push(make_rtp(0, 444, 10, 0));  // exact duplicate
  ing.push(make_rtp(0, 444, 300, 0));  // force window forward
  ing.push(make_rtp(0, 444, 11, 0));  // late, after its gap expired
  auto r = ing.finish(0);
  CHECK(r.stats.at(444).duplicates >= 1);
  CHECK(r.stats.at(444).late_unrecoverable >= 1);
}

TEST_CASE("Oversized and malformed datagrams are rejected per track",
          "[ingest]") {
  auto t = video_track(0, 555);
  RtpIngest ing({t});
  RawRecord big = make_rtp(0, 555, 0, 0);
  big.body.resize(kMaxPacketBytes + 1, 0);
  ing.push(big);
  RawRecord tiny;
  tiny.type = RecordType::Rtp;
  tiny.track_id = 0;
  tiny.body = {0x80};
  ing.push(tiny);
  auto r = ing.finish(0);
  CHECK(r.stats.at(555).invalid == 2);
  CHECK(r.packets.empty());
}

#include "timeline.hpp"
#include "track_graph.hpp"
#include "fixture.hpp"
#include "pipeline.hpp"
#include "silence.hpp"

TEST_CASE("Timeline maps RTP clock to nanoseconds via SR", "[timeline]") {
  TrackInfo t = video_track(0, 777);
  Timeline tl({t});
  // SR: rtp 9000 -> wall 1s (after NTP epoch conversion).
  RawRecord sr;
  sr.type = RecordType::RtcpSr;
  sr.track_id = 0;
  RtcpSrRecord rec{};
  rec.ssrc = 777;
  rec.rtp_timestamp = 9000;
  u64 wall_ns = u64(kNsPerSec);  // 1 second Unix
  rec.ntp_seconds = u32(wall_ns / u64(kNsPerSec)) + 2208988800u;
  rec.ntp_fraction = 0;
  sr.body.resize(sizeof(rec));
  std::memcpy(sr.body.data(), &rec, sizeof(rec));
  tl.add_sr(sr);
  std::vector<PacketRecord> pk;
  PacketRecord p;
  p.ssrc = 777;
  // At 90 kHz, +90000 RTP ticks = +1 wall second beyond the SR anchor.
  p.rtp_ts = 9000 + 90000;
  pk.push_back(p);
  std::vector<ErrorRecord> errs;
  REQUIRE(tl.map_packets(pk, errs));
  CHECK(pk[0].mapped_ns == 2 * kNsPerSec);
  CHECK(pk[0].uncertainty_ns > 0);
}

TEST_CASE("Timeline flags missing-SR tracks as estimated and merge-blocked",
          "[timeline]") {
  TrackInfo t = video_track(0, 888);
  Timeline tl({t});
  std::vector<PacketRecord> pk(3);
  for (std::size_t i = 0; i < 3; ++i) {
    pk[i].ssrc = 888;
    pk[i].rtp_ts = u32(i * 3000);
  }
  std::vector<ErrorRecord> errs;
  tl.map_packets(pk, errs);
  tl.evaluate_drift();
  CHECK(tl.stats().at(888).estimated);
  CHECK(tl.mutable_tracks()[0].merge_blocked);
}

TEST_CASE("Track graph rejects cycles and topologically orders DAG",
          "[graph]") {
  TrackGraph g;
  TrackNode a;
  a.track_id = 0; a.name = "a"; a.kind = TrackKind::Video;
  TrackNode b;
  b.track_id = 1; b.name = "b"; b.kind = TrackKind::Video;
  TrackNode c;
  c.track_id = 2; c.name = "c"; c.kind = TrackKind::Audio;
  g.add_node(a);
  g.add_node(b);
  g.add_node(c);
  g.add_dependency(1, 0);
  g.add_dependency(2, 1);
  auto s = g.topological_sort();
  REQUIRE(s.ok);
  CHECK(s.order == std::vector<u32>{0, 1, 2});

  TrackGraph cyc;
  TrackNode x;
  x.track_id = 0; x.name = "x"; x.kind = TrackKind::Video;
  TrackNode y;
  y.track_id = 1; y.name = "y"; y.kind = TrackKind::Video;
  cyc.add_node(x);
  cyc.add_node(y);
  cyc.add_dependency(0, 1);
  cyc.add_dependency(1, 0);
  auto cs = cyc.topological_sort();
  CHECK_FALSE(cs.ok);
  CHECK(cs.cycle_detail.find("cycle") != std::string::npos);
}

TEST_CASE("Failed upstream task causes downstream skip", "[graph]") {
  TrackGraph g;
  TrackNode a;
  a.track_id = 0; a.name = "a"; a.kind = TrackKind::Video;
  TrackNode b;
  b.track_id = 1; b.name = "b"; b.kind = TrackKind::Audio;
  g.add_node(a);
  g.add_node(b);
  g.add_dependency(1, 0);
  std::map<u32, std::function<bool()>> jobs;
  jobs[0] = [] { return false; };  // upstream failure
  jobs[1] = [] { return true; };
  auto s = g.run(jobs, 2);
  CHECK(std::find(s.failed.begin(), s.failed.end(), 0u) != s.failed.end());
  CHECK(std::find(s.skipped.begin(), s.skipped.end(), 1u) != s.skipped.end());
}

TEST_CASE("Silence detector finds the two quiet intervals per channel",
          "[silence]") {
  auto fx = FixtureGenerator(FixtureOptions{}).generate();
  auto s = Pipeline(PipelineConfig{}).run(fx.capture);
  auto it = std::find_if(s.outcomes.begin(), s.outcomes.end(),
                         [](const TrackOutcome& o) {
                           return o.info.kind == TrackKind::Audio;
                         });
  REQUIRE(it != s.outcomes.end());
  // Two ~400 ms regions on two channels; union >= 600 ms after losses.
  CHECK(it->silence.silence_ns >= 600 * 1'000'000);
  CHECK(it->silence.ranges.size() >= 2);
  for (const auto& r : it->silence.ranges)
    CHECK((r.end_ns - r.start_ns) >= kMinSilenceNs);
}

TEST_CASE("Pipeline is deterministic across reruns and meets thresholds",
          "[pipeline]") {
  PipelineConfig cfg;
  auto fx1 = FixtureGenerator(FixtureOptions{}).generate();
  auto fx2 = FixtureGenerator(FixtureOptions{}).generate();
  REQUIRE(fx1.capture == fx2.capture);  // fixture itself is deterministic
  auto a = Pipeline(cfg).run(fx1.capture);
  auto b = Pipeline(cfg).run(fx2.capture);
  REQUIRE(a.segment_rows.size() == b.segment_rows.size());
  for (std::size_t i = 0; i < a.segment_rows.size(); ++i) {
    CHECK(a.segment_rows[i].first == b.segment_rows[i].first);
    CHECK(a.segment_rows[i].second.start_ns ==
          b.segment_rows[i].second.start_ns);
  }
  CHECK(a.roundtrip_ratio >= 0.99);
  CHECK(a.keyframe_hit_ratio >= 0.95);
  CHECK(a.dedup.exact_pairs > 0);
  CHECK(a.dedup.candidate_pairs > 0);
}

TEST_CASE("Truncated capture still yields segments with truncation flag",
          "[pipeline]") {
  auto fx = FixtureGenerator(FixtureOptions{}).generate();
  auto cut = fx.capture;
  cut.resize(cut.size() * 90 / 100);  // cut deeper, no EOS
  auto s = Pipeline(PipelineConfig{}).run(cut);
  CHECK(s.ok);
  CHECK(!s.segment_rows.empty());
  bool any_trunc = std::any_of(
      s.segment_rows.begin(), s.segment_rows.end(),
      [](const auto& row) { return row.second.truncated; });
  CHECK(any_trunc);
}
