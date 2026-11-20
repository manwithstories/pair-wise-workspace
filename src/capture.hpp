#pragma once
// Self-contained capture container: a sequence of typed records carrying
// RTP/RTCP datagrams, optional index pages and an end-of-stream marker.
// All multi-byte integers are little-endian on disk.
#include <functional>
#include <map>
#include <string>
#include <vector>

#include "common.hpp"

namespace mb {

inline constexpr char kCapMagic[8] = {'M', 'B', 'C', 'A', 'P', '1', 0, 0};
inline constexpr u32 kCapVersion = 1;

enum class RecordType : u16 {
  TrackDecl = 1,
  Rtp = 2,
  RtcpSr = 3,
  IndexPage = 4,
  Eos = 5,
};

#pragma pack(push, 1)
struct CapHeader {
  char magic[8];
  u32 version;
  u32 flags;       // bit0: truncated tail, bit1: missing index pages
  u64 seed;
  u16 track_count;
};
struct RecordHeader {
  u16 type;
  u16 track_id;
  u32 length;      // payload length following this header
};
struct TrackDeclRecord {
  u32 ssrc;
  u8 kind;         // TrackKind
  u32 clock_rate;
  u16 channels;
};
struct RtpHeaderOnWire {
  u8 vpxcc;        // version(2)<<6 | payload[1]<<5 | csrc count
  u8 mpt;          // marker<<7 | payload type
  u16 seq;
  u32 timestamp;
  u32 ssrc;
};
struct RtcpSrRecord {
  u32 ssrc;
  u32 ntp_seconds;       // NTP seconds (1900 epoch)
  u32 ntp_fraction;
  u32 rtp_timestamp;
};
struct IndexEntry {
  u32 rtp_timestamp;
  u64 byte_offset;
  u8 keyframe;
};
#pragma pack(pop)

static_assert(sizeof(CapHeader) == 26);
static_assert(sizeof(RecordHeader) == 8);
static_assert(sizeof(RtpHeaderOnWire) == 12);

struct RawRecord {
  RecordType type;
  u16 track_id;
  u64 byte_offset;  // offset of the record header in the file
  std::vector<u8> body;
};

class CaptureWriter {
 public:
  CaptureWriter(u64 seed, u32 flags, u16 track_count);
  void write_track_decl(const TrackInfo& t);
  void write_rtp(u16 track_id, const PacketRecord& p);
  void write_rtcp_sr(u16 track_id, u32 ssrc, u32 ntp_sec, u32 ntp_frac,
                     u32 rtp_ts);
  void write_index_page(u16 track_id, const std::vector<IndexEntry>& page);
  void write_eos();
  const std::vector<u8>& bytes() const { return data_; }

 private:
  void begin_record(RecordType t, u16 track_id, u32 length);
  std::vector<u8> data_;
};

class CaptureReader {
 public:
  explicit CaptureReader(std::vector<u8> bytes) : data_(std::move(bytes)) {}
  // Parses the header; returns false on a malformed container.
  bool open(ErrorRecord& err);
  // Iterates records until EOS or truncation. Truncated tail -> error record
  // with Recoverability::Recovered and iteration stops cleanly.
  void for_each(const std::function<void(const RawRecord&)>& on_record,
                std::vector<ErrorRecord>& errors) const;
  const CapHeader& header() const { return header_; }
  u64 size() const { return data_.size(); }

 private:
  std::vector<u8> data_;
  CapHeader header_{};
};

}  // namespace mb
