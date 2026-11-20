#include "rtp_ingest.hpp"

#include <algorithm>
#include <cstring>

namespace mb {

RtpIngest::RtpIngest(const std::vector<TrackInfo>& tracks) {
  for (const auto& t : tracks) {
    buffers_.emplace(t.track_id, Buffer(t));
    ssrc_to_track_[t.ssrc] = static_cast<u16>(t.track_id);
  }
}

RtpIngest::Buffer& RtpIngest::buffer_for(u16 track_id, u32 ssrc) {
  auto it = buffers_.find(track_id);
  if (it == buffers_.end()) {  // unknown SSRC: declare a defensive track
    TrackInfo t;
    t.track_id = track_id;
    t.ssrc = ssrc;
    t.kind = TrackKind::Video;
    t.clock_rate = 90000;
    t.name = "unknown-" + std::to_string(ssrc);
    it = buffers_.emplace(track_id, Buffer(t)).first;
    ssrc_to_track_[ssrc] = track_id;
  }
  return it->second;
}

void RtpIngest::push_rtp(u16 track_id, const RawRecord& rec) {
  if (rec.body.size() > kMaxPacketBytes) {
    errors_.push_back({track_id, rec.byte_offset, {}, {}, "rtp.oversized",
                       "datagram exceeds maximum packet length",
                       Recoverability::Unrecoverable});
    auto& b = buffer_for(track_id, 0);
    b.stats.invalid++;
    return;
  }
  if (rec.body.size() < sizeof(RtpHeaderOnWire) + 1) {
    errors_.push_back({track_id, rec.byte_offset, {}, {}, "rtp.truncated",
                       "datagram smaller than RTP header",
                       Recoverability::Recovered});
    buffer_for(track_id, 0).stats.invalid++;
    return;
  }
  RtpHeaderOnWire h{};
  std::memcpy(&h, rec.body.data(), sizeof(h));
  if ((h.vpxcc >> 6) != kRtpVersion) {
    errors_.push_back({track_id, rec.byte_offset, {}, {}, "rtp.bad_version",
                       "RTP version field is not 2",
                       Recoverability::Recovered});
    buffer_for(track_id, h.ssrc).stats.invalid++;
    return;
  }
  Buffer& b = buffer_for(track_id, h.ssrc);
  u32 seq = rd16(reinterpret_cast<const u8*>(&h.seq));
  u64 abs_seq;
  if (!b.last_seq) {
    abs_seq = seq;  // first packet anchors the extended sequence space
  } else {
    // Modular delta keeps wrap arithmetic inside the 16-bit range.
    abs_seq = b.high_water + static_cast<u64>(seqDelta(seq, *b.last_seq));
  }
  if (!b.next_to_emit) b.next_to_emit = abs_seq;
  bool corrupt = rec.body[sizeof(h)] == 0xFF;
  if (corrupt) {
    b.stats.corrupt++;
    errors_.push_back({track_id, rec.byte_offset, h.ssrc, seq, "rtp.corrupt",
                       "payload marked corrupt by capture",
                       Recoverability::Recovered});
  }
  if (b.pending.contains(abs_seq) ||
      (b.next_to_emit && abs_seq < *b.next_to_emit)) {
    b.stats.duplicates++;
    if (abs_seq < *b.next_to_emit) {
      b.stats.late_unrecoverable++;
      errors_.push_back({track_id, rec.byte_offset, h.ssrc, seq,
                         "rtp.late", "packet arrived after its gap expired",
                         Recoverability::Unrecoverable});
    }
    return;
  }
  PacketRecord p;
  p.ssrc = h.ssrc;
  p.seq = seq;
  p.abs_seq = abs_seq;
  p.rtp_ts = rd32(reinterpret_cast<const u8*>(&h.timestamp));
  p.payload_type = h.mpt & 0x7Fu;
  p.marker = (h.mpt & 0x80u) != 0;
  p.keyframe = rec.body[sizeof(h)] == 0x01;
  p.payload.assign(rec.body.begin() + static_cast<std::ptrdiff_t>(sizeof(h) + 1),
                   rec.body.end());
  p.byte_offset = rec.byte_offset;
  p.corrupt = corrupt;
  p.recovery = corrupt ? Recoverability::Recovered : Recoverability::Ok;
  b.stats.received++;
  b.pending.emplace(abs_seq, std::move(p));
  if (!b.last_seq || seqDelta(seq, *b.last_seq) > 0) b.last_seq = seq;
  if (abs_seq > b.high_water) b.high_water = abs_seq;
  // Expire gaps that have fallen outside the reorder window immediately so
  // late packets can be classified as unrecoverable on arrival.
  u64 floor = (b.high_water > kReorderWindow)
                  ? b.high_water - kReorderWindow + 1
                  : 0;
  if (b.next_to_emit && *b.next_to_emit < floor) {
    u64 old_next = *b.next_to_emit;
    *b.next_to_emit = floor;
    b.stats.lost += static_cast<u64>(floor - old_next);
    for (u64 s = old_next; s < floor; ++s) {
      errors_.push_back({b.track.track_id, 0, b.track.ssrc,
                         static_cast<u32>(s % kSeqMod), "rtp.gap",
                         "sequence gap left reorder window",
                         Recoverability::Unrecoverable});
    }
  }
}

void RtpIngest::emit_ready(Buffer& b, std::vector<PacketRecord>& out) {
  // Walk the ordered map; any missing sequence below the next key is a gap.
  // Gaps already expired on arrival were counted in push_rtp(), so this loop
  // only fills holes that were inside the reorder window.
  for (auto& [seq, pkt] : b.pending) {
    while (*b.next_to_emit < seq) {
      // The high-water packet had already arrived when this gap closed, so
      // the distance measures the reorder depth the buffer absorbed.
      u32 depth = static_cast<u32>(b.high_water - *b.next_to_emit + 1);
      b.stats.max_reorder_depth = std::max(b.stats.max_reorder_depth, depth);
      b.stats.lost++;
      errors_.push_back({b.track.track_id, 0, b.track.ssrc,
                         static_cast<u32>(*b.next_to_emit % kSeqMod),
                         "rtp.gap_eos", "gap at end of stream",
                         Recoverability::Unrecoverable});
      (*b.next_to_emit)++;
    }
    out.push_back(std::move(pkt));
    (*b.next_to_emit)++;
  }
  b.pending.clear();
}

void RtpIngest::push(const RawRecord& rec) {
  switch (rec.type) {
    case RecordType::Rtp:
      push_rtp(rec.track_id, rec);
      break;
    case RecordType::RtcpSr:
      rtcp_.push_back(rec);
      break;
    default:
      break;
  }
}

IngestResult RtpIngest::finish(u64 capture_size) {
  IngestResult r;
  r.rtcp = std::move(rtcp_);
  r.errors = std::move(errors_);
  (void)capture_size;  // tail gaps reference offset 0; file length known upstream
  for (auto& [id, b] : buffers_) {
    (void)id;
    if (!b.next_to_emit) {
      r.stats.emplace(b.track.ssrc, b.stats);
      continue;
    }
    u64 first_seq = *b.next_to_emit;
    emit_ready(b, r.packets);
    b.stats.first_abs_seq = first_seq;
    b.stats.last_abs_seq = b.high_water;
    r.stats.emplace(b.track.ssrc, b.stats);
  }
  return r;
}

}  // namespace mb
