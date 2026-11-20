#include "capture.hpp"

#include <cstring>

namespace mb {

namespace {
void append_bytes(std::vector<u8>& dst, const void* src, std::size_t n) {
  const auto* p = static_cast<const u8*>(src);
  dst.insert(dst.end(), p, p + n);
}
}  // namespace

CaptureWriter::CaptureWriter(u64 seed, u32 flags, u16 track_count) {
  CapHeader h{};
  std::memcpy(h.magic, kCapMagic, sizeof(kCapMagic));
  h.version = kCapVersion;
  h.flags = flags;
  h.seed = seed;
  h.track_count = track_count;
  append_bytes(data_, &h, sizeof(h));
}

void CaptureWriter::begin_record(RecordType t, u16 track_id, u32 length) {
  RecordHeader rh{static_cast<u16>(t), track_id, length};
  append_bytes(data_, &rh, sizeof(rh));
}

void CaptureWriter::write_track_decl(const TrackInfo& t) {
  TrackDeclRecord r{t.ssrc, static_cast<u8>(t.kind), t.clock_rate,
                    t.channels};
  begin_record(RecordType::TrackDecl, static_cast<u16>(t.track_id),
               static_cast<u32>(sizeof(r)));
  u32 name_len = static_cast<u32>(t.name.size());
  append_bytes(data_, &r, sizeof(r));
  wr32(data_, name_len);
  append_bytes(data_, t.name.data(), t.name.size());
}

void CaptureWriter::write_rtp(u16 track_id, const PacketRecord& p) {  RtpHeaderOnWire h{};
  h.vpxcc = static_cast<u8>(kRtpVersion << 6);
  h.mpt = static_cast<u8>((p.marker ? 0x80u : 0u) |
                          (p.payload_type & 0x7Fu));
  h.seq = static_cast<u16>(p.seq);
  h.timestamp = p.rtp_ts;
  h.ssrc = p.ssrc;
  // One extension byte carries the keyframe flag for our synthetic codec.
  const u8 ext = static_cast<u8>(p.keyframe ? 0x01 : 0x00);
  begin_record(RecordType::Rtp, track_id,
               static_cast<u32>(sizeof(h) + 1 + p.payload.size()));
  append_bytes(data_, &h, sizeof(h));
  append_bytes(data_, &ext, 1);
  append_bytes(data_, p.payload.data(), p.payload.size());
}

void CaptureWriter::write_rtcp_sr(u16 track_id, u32 ssrc, u32 ntp_sec,
                                  u32 ntp_frac, u32 rtp_ts) {
  RtcpSrRecord r{ssrc, ntp_sec, ntp_frac, rtp_ts};
  begin_record(RecordType::RtcpSr, track_id, sizeof(r));
  append_bytes(data_, &r, sizeof(r));
}

void CaptureWriter::write_index_page(u16 track_id,
                                     const std::vector<IndexEntry>& page) {
  begin_record(RecordType::IndexPage, track_id,
               static_cast<u32>(page.size() * sizeof(IndexEntry)));
  append_bytes(data_, page.data(), page.size() * sizeof(IndexEntry));
}

void CaptureWriter::write_eos() {
  begin_record(RecordType::Eos, 0, 0);
}

bool CaptureReader::open(ErrorRecord& err) {
  if (data_.size() < sizeof(CapHeader)) {
    err = {0, data_.size(), {}, {}, "capture.truncated_header",
           "file shorter than container header", Recoverability::Unrecoverable};
    return false;
  }
  std::memcpy(&header_, data_.data(), sizeof(header_));
  if (std::memcmp(header_.magic, kCapMagic, sizeof(kCapMagic)) != 0 ||
      header_.version != kCapVersion) {
    err = {0, 0, {}, {}, "capture.bad_magic", "magic/version mismatch",
           Recoverability::Unrecoverable};
    return false;
  }
  return true;
}

void CaptureReader::for_each(
    const std::function<void(const RawRecord&)>& on_record,
    std::vector<ErrorRecord>& errors) const {
  u64 off = sizeof(CapHeader);
  const u64 total = data_.size();
  while (off < total) {
    if (total - off < sizeof(RecordHeader)) {
      errors.push_back({0, off, {}, {}, "capture.truncated_record",
                        "record header cut off at tail",
                        Recoverability::Recovered});
      return;
    }
    RecordHeader rh{};
    std::memcpy(&rh, data_.data() + off, sizeof(rh));
    u64 body_off = off + sizeof(RecordHeader);
    if (u64(rh.length) > total - body_off) {
      errors.push_back({rh.track_id, off, {}, {}, "capture.truncated_body",
                        "record body extends past end of file",
                        Recoverability::Recovered});
      return;
    }
    RawRecord rec{static_cast<RecordType>(rh.type), rh.track_id, off, {}};
    rec.body.assign(data_.begin() + static_cast<std::ptrdiff_t>(body_off),
                    data_.begin() +
                        static_cast<std::ptrdiff_t>(body_off + rh.length));
    on_record(rec);
    off = body_off + rh.length;
  }
}

}  // namespace mb
