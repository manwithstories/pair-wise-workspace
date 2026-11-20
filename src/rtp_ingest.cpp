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
  // Wrap-aware extension. A forward wrap happens only when the current
  // maximum sits near the modulus top and the new sequence restarts low;
  // everything else, however late, stays in the current cycle.
  if (!b.seen) {
    b.seen = true;
    b.max_seq = seq;
  } else if (b.wrap_count == 0 && seq < kReorderWindow &&
             b.max_seq > u32(kSeqMod) - kReorderWindow) {
    b.wrap_count++;
    // Keep the ceiling at the old cycle's top so late high-seq packets are
    // not mistaken for a second wrap.
    b.max_seq = u32(kSeqMod) - 1u;
  } else if (seq > b.max_seq) {
    b.max_seq = seq;
  }
  u64 abs_seq = u64(b.wrap_count) * u64(kSeqMod) + seq;
  if (!b.next_to_emit) b.next_to_emit = abs_seq;
  if (abs_seq > b.high_water) b.high_water = abs_seq;
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
  // Drain everything that can now be emitted, ageing out expired gaps.
  // Output is collected per buffer; finish() concatenates in track order.
  emit_ready(b, drained_);
}

void RtpIngest::emit_ready(Buffer& b, std::vector<PacketRecord>& out) {
  if (!b.next_to_emit || b.pending.empty()) return;
  // A gap at sequence S is declared lost only once the buffer has advanced
  // kReorderWindow packets beyond S, i.e. the high-water mark passes
  // S + kReorderWindow. Until then a late packet may still fill the hole.
  u64 expire_floor = (b.high_water >= kReorderWindow)
                         ? b.high_water - kReorderWindow + 1
                         : 0;
  while (!b.pending.empty()) {
    u64 head = b.pending.begin()->first;
    while (*b.next_to_emit < head && *b.next_to_emit < expire_floor) {
      b.stats.lost++;
      errors_.push_back({b.track.track_id, 0, b.track.ssrc,
                         static_cast<u32>(*b.next_to_emit % kSeqMod),
                         "rtp.gap", "sequence gap left reorder window",
                         Recoverability::Unrecoverable});
      (*b.next_to_emit)++;
    }
    if (head != *b.next_to_emit) break;  // gap still inside the window
    if (b.high_water > head) {
      u32 depth = static_cast<u32>(b.high_water - head + 1);
      b.stats.max_reorder_depth = std::max(b.stats.max_reorder_depth, depth);
    }
    out.push_back(std::move(b.pending.begin()->second));
    b.pending.erase(b.pending.begin());
    (*b.next_to_emit)++;
  }
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
  for (auto& [id, b] : buffers_) {
    (void)id;
    if (!b.next_to_emit) {
      r.stats.emplace(b.track.ssrc, b.stats);
      continue;
    }
    u64 first_seq = *b.next_to_emit;
    emit_ready(b, r.packets);
    // Anything still buffered at EOS: flush in order, recording tail gaps.
    while (!b.pending.empty()) {
      u64 head = b.pending.begin()->first;
      while (*b.next_to_emit < head) {
        b.stats.lost++;
        r.errors.push_back({b.track.track_id, capture_size, b.track.ssrc,
                            static_cast<u32>(*b.next_to_emit % kSeqMod),
                            "rtp.gap_eos", "gap at end of stream",
                            Recoverability::Unrecoverable});
        (*b.next_to_emit)++;
      }
      r.packets.push_back(std::move(b.pending.begin()->second));
      b.pending.erase(b.pending.begin());
      (*b.next_to_emit)++;
    }
    b.stats.first_abs_seq = first_seq;
    b.stats.last_abs_seq = b.high_water;
    r.stats.emplace(b.track.ssrc, b.stats);
  }
  // Prepend packets drained during push (already in per-buffer seq order).
  r.packets.insert(r.packets.begin(),
                   std::make_move_iterator(drained_.begin()),
                   std::make_move_iterator(drained_.end()));
  drained_.clear();
  return r;
}

}  // namespace mb
