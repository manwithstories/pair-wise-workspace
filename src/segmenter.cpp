#include "segmenter.hpp"

#include <algorithm>
#include <cstring>

namespace mb {

Segmenter::Segmenter(TrackInfo track, bool capture_truncated,
                     bool index_missing)
    : track_(std::move(track)),
      capture_truncated_(capture_truncated),
      index_missing_(index_missing) {}

void Segmenter::load_index_pages(const std::vector<RawRecord>& pages) {
  for (const auto& rec : pages) {
    std::size_t n = rec.body.size() / sizeof(IndexEntry);
    for (std::size_t i = 0; i < n; ++i) {
      IndexEntry e{};
      std::memcpy(&e, rec.body.data() + i * sizeof(IndexEntry), sizeof(e));
      if (e.keyframe) index_.push_back({e.rtp_timestamp, e.byte_offset});
    }
  }
  std::sort(index_.begin(), index_.end(),
            [](const KeyIndex& a, const KeyIndex& b) {
              return a.rtp_ts < b.rtp_ts;
            });
}

void Segmenter::rebuild_index(const std::vector<PacketRecord>& packets) {
  index_.clear();
  for (const auto& p : packets) {
    if (!p.corrupt && p.keyframe)
      index_.push_back({p.rtp_ts, p.byte_offset});
  }
}

bool Segmenter::has_keyframe_near(i64 target_ns,
                                  const PacketRecord& head) const {
  if (track_.kind == TrackKind::Audio) return true;  // audio needs no IDR
  if (head.keyframe &&
      std::llabs(head.mapped_ns - target_ns) <= kKeyframeToleranceNs)
    return true;
  return std::any_of(index_.begin(), index_.end(), [&](const KeyIndex& k) {
    return k.byte_offset == head.byte_offset;
  });
}

SegmenterResult Segmenter::run(
    const std::vector<PacketRecord>& packets) {
  SegmenterResult out;
  if (packets.empty()) return out;

  bool rebuilt = false;
  if (index_missing_ ||
      (track_.kind == TrackKind::Video && index_.empty())) {
    rebuild_index(packets);
    rebuilt = true;
    out.rebuilt_pages++;
  }

  struct Open {
    Segment seg;
    const PacketRecord* head = nullptr;
  } open;
  i64 boundary = packets.front().mapped_ns + kSegmentTargetNs;
  open.seg.track_id = track_.track_id;
  open.seg.ssrc = track_.ssrc;
  open.seg.index_rebuilt = rebuilt;
  open.seg.start_ns = packets.front().mapped_ns;
  open.seg.source_offset_begin = packets.front().byte_offset;
  open.seg.first_seq = packets.front().seq;
  open.head = &packets.front();

  auto append_canonical = [](Segment& s, const PacketRecord& p) {
    u8 hdr[4] = {u8(p.rtp_ts >> 24), u8(p.rtp_ts >> 16),
                 u8(p.rtp_ts >> 8), u8(p.rtp_ts)};
    s.normalized.insert(s.normalized.end(), hdr, hdr + 4);
    s.normalized.insert(s.normalized.end(), p.payload.begin(),
                        p.payload.end());
  };
  auto flush = [&](const PacketRecord* tail, bool truncated) {
    if (open.seg.normalized.empty()) return;
    open.seg.end_ns = tail ? tail->mapped_ns : open.seg.start_ns;
    open.seg.source_offset_end =
        tail ? tail->byte_offset : open.seg.source_offset_begin;
    open.seg.last_seq = tail ? tail->seq : open.seg.first_seq;
    open.seg.truncated = truncated;
    open.seg.keyframe_hit =
        has_keyframe_near(open.seg.start_ns, *open.head);
    if (open.seg.keyframe_hit)
      out.keyframe_index_hits++;
    else if (track_.kind == TrackKind::Video)
      out.keyframe_index_misses++;
    out.segments.push_back(std::move(open.seg));
  };

  for (const auto& p : packets) {
    bool crossed = p.mapped_ns >= boundary;
    bool at_key = track_.kind == TrackKind::Audio || p.keyframe;
    if (crossed && at_key) {
      flush(&p, false);
      open = Open{};
      open.seg.track_id = track_.track_id;
      open.seg.ssrc = track_.ssrc;
      open.seg.index_rebuilt = rebuilt;
      open.seg.start_ns = p.mapped_ns;
      open.seg.source_offset_begin = p.byte_offset;
      open.seg.first_seq = p.seq;
      open.head = &p;
      boundary = p.mapped_ns + kSegmentTargetNs;
    }
    if (p.recovery == Recoverability::Unrecoverable || p.corrupt) {
      open.seg.has_gap = true;
      open.seg.errors.push_back(
          {track_.track_id, p.byte_offset, p.ssrc, p.seq,
           p.corrupt ? "segment.corrupt_packet" : "segment.gap",
           "packet defect carried into segment metadata", p.recovery});
    }
    append_canonical(open.seg, p);
  }
  flush(nullptr, capture_truncated_);

  for (auto& s : out.segments) {
    if (s.end_ns - s.start_ns < kMinSegmentNs) s.truncated = true;
  }
  return out;
}

}  // namespace mb
