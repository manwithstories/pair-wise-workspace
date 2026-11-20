#include "pipeline.hpp"

#include <algorithm>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <mutex>
#include <numeric>
#include <set>

#include "fixture.hpp"
#include "rtp_ingest.hpp"
#include "sha256.hpp"

namespace mb {

namespace fs = std::filesystem;

bool atomic_write(const std::string& path, const std::vector<u8>& bytes) {
  std::string tmp = path + ".tmp";
  std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
  if (!f) return false;
  f.write(reinterpret_cast<const char*>(bytes.data()),
          static_cast<std::streamsize>(bytes.size()));
  f.close();
  if (!f) return false;
  std::error_code ec;
  fs::rename(tmp, path, ec);
  return !ec;
}

bool atomic_write_text(const std::string& path, std::string_view text) {
  return atomic_write(path, std::vector<u8>(text.begin(), text.end()));
}

TrackOutcome Pipeline::process_track(
    const TrackInfo& track, const std::vector<PacketRecord>& packets,
    const std::vector<RawRecord>& index_pages, bool capture_truncated,
    bool index_missing) {
  TrackOutcome out;
  out.info = track;
  out.packets_in = packets.size();
  Segmenter seg(track, capture_truncated, index_missing);
  seg.load_index_pages(index_pages);
  SegmenterResult sr = seg.run(packets);
  out.segments = std::move(sr.segments);
  if (track.kind == TrackKind::Audio) {
    SilenceDetector det(track.clock_rate, track.channels);
    out.silence = det.analyse(out.segments, packets);
  }
  if (out.segments.empty()) {
    out.status = TaskStatus::Failed;
    out.detail = "no decodable segment produced";
  } else {
    out.status = TaskStatus::Done;
  }
  return out;
}

PipelineSummary Pipeline::run(const std::vector<u8>& capture_bytes) {
  PipelineSummary sum;
  CaptureReader reader(capture_bytes);
  ErrorRecord open_err;
  if (!reader.open(open_err)) {
    sum.errors.push_back(open_err);
    sum.failed = 1;
    return sum;
  }
  bool truncated = (reader.header().flags & 1u) != 0;
  bool index_missing = (reader.header().flags & 2u) != 0;

  std::vector<RawRecord> records;
  std::vector<ErrorRecord> parse_errors;
  reader.for_each([&](const RawRecord& r) { records.push_back(r); },
                  parse_errors);
  sum.errors.insert(sum.errors.end(), parse_errors.begin(), parse_errors.end());

  std::vector<TrackInfo> tracks;
  std::vector<RawRecord> index_pages;
  for (const auto& r : records) {
    if (r.type == RecordType::TrackDecl) {
      TrackDeclRecord d{};
      std::memcpy(&d, r.body.data(), sizeof(d));
      u32 name_len = rd32(r.body.data() + sizeof(d));
      TrackInfo t;
      t.track_id = r.track_id;
      t.ssrc = d.ssrc;
      t.kind = TrackKind(d.kind);
      t.clock_rate = d.clock_rate;
      t.channels = d.channels;
      t.name.assign(r.body.begin() + static_cast<std::ptrdiff_t>(
                                         sizeof(d) + sizeof(u32)),
                    r.body.begin() + static_cast<std::ptrdiff_t>(
                                         sizeof(d) + sizeof(u32) + name_len));
      tracks.push_back(t);
    } else if (r.type == RecordType::IndexPage) {
      index_pages.push_back(r);
    }
  }

  RtpIngest ingest(tracks);
  for (const auto& r : records) ingest.push(r);
  IngestResult ir = ingest.finish(capture_bytes.size());
  sum.ingest = ir.stats;
  sum.errors.insert(sum.errors.end(), ir.errors.begin(), ir.errors.end());

  Timeline timeline(tracks);
  for (const auto& sr_rec : ir.rtcp) timeline.add_sr(sr_rec);
  timeline.map_packets(ir.packets, sum.errors);
  timeline.evaluate_drift();
  tracks = timeline.mutable_tracks();

  // Group packets and index pages per track for deterministic processing.
  std::map<u32, std::vector<PacketRecord>> per_track;
  for (auto& p : ir.packets) {
    auto it = std::find_if(tracks.begin(), tracks.end(),
                           [&](const TrackInfo& t) { return t.ssrc == p.ssrc; });
    if (it != tracks.end()) per_track[it->track_id].push_back(std::move(p));
  }
  std::map<u32, std::vector<RawRecord>> pages;
  for (const auto& pg : index_pages) pages[pg.track_id].push_back(pg);

  TrackGraph graph;
  for (const auto& t : tracks) {
    TrackNode n;
    n.track_id = t.track_id;
    n.name = t.name;
    n.kind = t.kind;
    if (t.kind == TrackKind::Audio && !tracks.empty()) {
      // Audio syncs to the first video track (A/V sync dependency).
      auto v = std::find_if(tracks.begin(), tracks.end(), [](const TrackInfo& x) {
        return x.kind == TrackKind::Video;
      });
      if (v != tracks.end() && v->track_id != t.track_id)
        n.depends_on.push_back(v->track_id);
    }
    graph.add_node(n);
  }
  std::map<u32, std::function<bool()>> jobs;
  std::map<u32, TrackOutcome> produced;
  std::mutex prod_m;
  for (const auto& t : tracks) {
    jobs[t.track_id] = [&, t]() -> bool {
      TrackOutcome o = process_track(
          t, per_track[t.track_id], pages[t.track_id], truncated,
          index_missing);
      std::lock_guard<std::mutex> lk(prod_m);
      produced[t.track_id] = std::move(o);
      return produced[t.track_id].status == TaskStatus::Done;
    };
  }
  GraphSchedule gs = graph.run(jobs, cfg_.max_workers);
  if (!gs.ok) {
    sum.errors.push_back({0, 0, {}, {}, "graph.cycle", gs.cycle_detail,
                          Recoverability::Unrecoverable});
    sum.failed = tracks.size();
    return sum;
  }

  // Assemble outcomes in deterministic track order with unique segment ids.
  u32 seg_id = 0;
  std::map<u32, TrackInfo> track_map;
  for (const auto& t : tracks) track_map[t.track_id] = t;
  for (const auto& t : tracks) {
    auto it = produced.find(t.track_id);
    if (it == produced.end()) {  // skipped due to upstream failure
      TrackOutcome skip;
      skip.info = t;
      skip.status = TaskStatus::Skipped;
      skip.detail = "upstream track failed";
      sum.outcomes.push_back(std::move(skip));
      continue;
    }
    TrackOutcome& o = it->second;
    for (auto& s : o.segments) {
      s.segment_id = seg_id++;
      sum.segment_rows.emplace_back(Sha256::hash(s.normalized), s);
    }
    for (const auto& s : o.segments) sum.keyframe_hits += s.keyframe_hit ? 1 : 0;
    for (const auto& s : o.segments)
      sum.keyframe_misses += (!s.keyframe_hit && t.kind == TrackKind::Video) ? 1 : 0;
    sum.silence_ms += u64(o.silence.silence_ns / 1'000'000);
    sum.outcomes.push_back(std::move(o));
  }

  // Dedup across tracks using the canonical hashes/fingerprints.
  std::vector<Segment> all_segments;
  for (const auto& row : sum.segment_rows) all_segments.push_back(row.second);
  sum.dedup = Fingerprinter::dedup(all_segments, track_map);

  sum.total_packets = std::accumulate(
      sum.ingest.begin(), sum.ingest.end(), u64(0),
      [](u64 acc, const auto& kv) {
        return acc + kv.second.received + kv.second.lost +
               kv.second.duplicates;
      });
  sum.emitted_packets = std::accumulate(
      sum.ingest.begin(), sum.ingest.end(), u64(0),
      [](u64 acc, const auto& kv) { return acc + kv.second.received; });
  sum.roundtrip_ratio = sum.total_packets
                            ? double(sum.emitted_packets) / double(sum.total_packets)
                            : 0.0;
  u64 kf_den = sum.keyframe_hits + sum.keyframe_misses;
  sum.keyframe_hit_ratio = kf_den ? double(sum.keyframe_hits) / double(kf_den)
                                  : 1.0;
  sum.segment_count = seg_id;
  sum.recovered = static_cast<u64>(std::count_if(
      sum.errors.begin(), sum.errors.end(),
      [](const ErrorRecord& e) { return e.recovery != Recoverability::Ok; }));
  std::set<u32> failed_tracks(gs.failed.begin(), gs.failed.end());
  failed_tracks.insert(gs.skipped.begin(), gs.skipped.end());
  sum.failed = failed_tracks.size();
  sum.ok = true;
  return sum;
}

}  // namespace mb
