#include "relay/frame_record.hpp"

#include <array>
#include <cstring>

extern "C" {
#include <libavutil/mem.h>
#include <libavutil/sha.h>
}

namespace relay {

namespace {
constexpr std::array<uint8_t, 8> kMagic{'R', 'L', 'Y', 'F', 'R', 'M', '0', '1'};
constexpr std::array<uint8_t, 8> kTrailer{'R', 'L', 'Y', 'E', 'N', 'D', '0', '1'};
constexpr uint32_t kVersion = 1;
// record_len(4) seq(8) clock(8) tb(4+4) flags(1) rsv(3) payload_len(4) sha(32)
constexpr uint32_t kRecordFixedLen = 4 + 8 + 8 + 4 + 4 + 1 + 3 + 4 + 32;
constexpr uint32_t kMaxRecordLen = 64u * 1024u * 1024u;

bool is_zero(const Sha256& s) {
  for (auto b : s)
    if (b != 0) return false;
  return true;
}

void put_u32(std::vector<uint8_t>& b, uint32_t v) {
  for (int i = 0; i < 4; ++i) b.push_back(static_cast<uint8_t>(v >> (i * 8)));
}
void put_u64(std::vector<uint8_t>& b, uint64_t v) {
  for (int i = 0; i < 8; ++i) b.push_back(static_cast<uint8_t>(v >> (i * 8)));
}
uint32_t get_u32(const uint8_t* p) {
  uint32_t v = 0;
  for (int i = 0; i < 4; ++i) v |= static_cast<uint32_t>(p[i]) << (i * 8);
  return v;
}
uint64_t get_u64(const uint8_t* p) {
  uint64_t v = 0;
  for (int i = 0; i < 8; ++i) v |= static_cast<uint64_t>(p[i]) << (i * 8);
  return v;
}
} // namespace

Sha256 sha256(const uint8_t* data, size_t len) {
  Sha256 out{};
  AVSHA* ctx = av_sha_alloc();
  if (!ctx) return out;
  av_sha_init(ctx, 256);
  av_sha_update(ctx, data, len);
  av_sha_final(ctx, out.data());
  av_free(ctx);
  return out;
}

// Checksum covers every serialized field from frame_seq through payload.
Sha256 record_checksum(const FrameRecord& rec) {
  std::vector<uint8_t> buf;
  buf.reserve(36 + rec.payload.size());
  put_u64(buf, rec.frame_seq);
  put_u64(buf, static_cast<uint64_t>(rec.node_clock));
  put_u32(buf, static_cast<uint32_t>(rec.time_base.num));
  put_u32(buf, static_cast<uint32_t>(rec.time_base.den));
  buf.push_back(rec.keyframe ? 1 : 0);
  buf.insert(buf.end(), {0, 0, 0});
  put_u32(buf, static_cast<uint32_t>(rec.payload.size()));
  buf.insert(buf.end(), rec.payload.begin(), rec.payload.end());
  return sha256(buf.data(), buf.size());
}

bool checksum_matches(const FrameRecord& rec) {
  return record_checksum(rec) == rec.checksum;
}

std::string hex(const Sha256& d) {
  static const char* k = "0123456789abcdef";
  std::string s;
  s.reserve(64);
  for (auto b : d) {
    s.push_back(k[b >> 4]);
    s.push_back(k[b & 0xf]);
  }
  return s;
}

const char* to_string(ParseError e) {
  switch (e) {
  case ParseError::None: return "none";
  case ParseError::BadMagic: return "bad_magic";
  case ParseError::TruncatedHeader: return "truncated_header";
  case ParseError::TruncatedField: return "truncated_field";
  case ParseError::InvalidLength: return "invalid_length";
  case ParseError::UnknownTimeBase: return "unknown_time_base";
  case ParseError::TruncatedTail: return "truncated_tail";
  }
  return "unknown";
}

ParseResult parse_record(const uint8_t* buf, size_t len, FrameRecord& out) {
  if (len < 4) return {ParseError::TruncatedField, 0, "record prefix short"};
  const uint32_t record_len = get_u32(buf);
  if (record_len < kRecordFixedLen || record_len > kMaxRecordLen)
    return {ParseError::InvalidLength, 0, "record length out of range"};
  if (len < record_len)
    return {ParseError::TruncatedField, 0, "record body truncated"};
  const uint8_t* p = buf + 4;
  out = {};
  out.frame_seq = get_u64(p);
  out.node_clock = static_cast<int64_t>(get_u64(p + 8));
  out.time_base.num = static_cast<int32_t>(get_u32(p + 16));
  out.time_base.den = static_cast<int32_t>(get_u32(p + 20));
  out.keyframe = (p[24] & 1) != 0;
  const uint32_t payload_len = get_u32(p + 28);
  if (kRecordFixedLen + payload_len != record_len)
    return {ParseError::InvalidLength, 0, "payload length mismatch"};
  out.payload.assign(p + 32, p + 32 + payload_len);
  std::memcpy(out.checksum.data(), p + 32 + payload_len, kSha256Len);
  return {};
}

// ---- Writer ----

FrameContainerWriter::FrameContainerWriter(const std::string& path)
    : f_(std::fopen(path.c_str(), "wb")) {}
FrameContainerWriter::~FrameContainerWriter() {
  if (f_) std::fclose(f_);
}
bool FrameContainerWriter::good() const { return f_ != nullptr && !std::ferror(f_); }

bool FrameContainerWriter::write_header(uint64_t declared_frames) {
  if (!f_) return false;
  if (std::fwrite(kMagic.data(), 1, kMagic.size(), f_) != kMagic.size()) return false;
  std::vector<uint8_t> b;
  put_u32(b, kVersion);
  put_u64(b, declared_frames);
  return std::fwrite(b.data(), 1, b.size(), f_) == b.size();
}

bool FrameContainerWriter::write_record(const FrameRecord& rec_in) {
  if (!f_) return false;
  FrameRecord rec = rec_in;
  if (is_zero(rec.checksum)) rec.checksum = record_checksum(rec);
  std::vector<uint8_t> b;
  put_u32(b, kRecordFixedLen + static_cast<uint32_t>(rec.payload.size()));
  put_u64(b, rec.frame_seq);
  put_u64(b, static_cast<uint64_t>(rec.node_clock));
  put_u32(b, static_cast<uint32_t>(rec.time_base.num));
  put_u32(b, static_cast<uint32_t>(rec.time_base.den));
  b.push_back(rec.keyframe ? 1 : 0);
  b.insert(b.end(), {0, 0, 0});
  put_u32(b, static_cast<uint32_t>(rec.payload.size()));
  b.insert(b.end(), rec.payload.begin(), rec.payload.end());
  b.insert(b.end(), rec.checksum.begin(), rec.checksum.end());
  return std::fwrite(b.data(), 1, b.size(), f_) == b.size();
}

bool FrameContainerWriter::write_trailer() {
  if (!f_) return false;
  return std::fwrite(kTrailer.data(), 1, kTrailer.size(), f_) == kTrailer.size() &&
         std::fflush(f_) == 0;
}

// ---- Reader ----

FrameContainerReader::FrameContainerReader(const std::string& path)
    : f_(std::fopen(path.c_str(), "rb")) {}
FrameContainerReader::~FrameContainerReader() {
  if (f_) std::fclose(f_);
}

ParseResult FrameContainerReader::open() {
  if (!f_) return {ParseError::BadMagic, 0, "cannot open file"};
  std::array<uint8_t, 8> magic{};
  if (std::fread(magic.data(), 1, 8, f_) != 8)
    return {ParseError::TruncatedHeader, 0, "short container header"};
  if (magic != kMagic) return {ParseError::BadMagic, 0, "bad container magic"};
  uint8_t hdr[12];
  if (std::fread(hdr, 1, 12, f_) != 12)
    return {ParseError::TruncatedHeader, 8, "short container header"};
  if (get_u32(hdr) != kVersion)
    return {ParseError::BadMagic, 8, "unsupported container version"};
  declared_ = get_u64(hdr + 4);
  offset_ = 20;
  opened_ = true;
  return {};
}

bool FrameContainerReader::next(FrameRecord& out) {
  err_ = {};
  if (!opened_) {
    err_ = {ParseError::BadMagic, 0, "reader not opened"};
    return false;
  }
  uint8_t first4[4];
  size_t got = std::fread(first4, 1, 4, f_);
  if (got == 0) {
    err_ = {ParseError::TruncatedTail, offset_, "missing trailer, EOF in container"};
    return false;
  }
  if (got < 4) {
    err_ = {ParseError::TruncatedTail, offset_, "truncated tail bytes"};
    return false;
  }
  if (std::memcmp(first4, kTrailer.data(), 4) == 0) {
    uint8_t rest[4];
    if (std::fread(rest, 1, 4, f_) == 4 &&
        std::memcmp(rest, kTrailer.data() + 4, 4) == 0) {
      offset_ += 8;
      return false; // clean EOF
    }
    err_ = {ParseError::TruncatedTail, offset_, "corrupt trailer"};
    return false;
  }
  const uint64_t rec_off = offset_;
  uint32_t record_len = get_u32(first4);
  if (record_len < kRecordFixedLen || record_len > kMaxRecordLen) {
    err_ = {ParseError::InvalidLength, rec_off, "record length out of range"};
    return false;
  }
  std::vector<uint8_t> raw(record_len);
  std::memcpy(raw.data(), first4, 4);
  if (std::fread(raw.data() + 4, 1, record_len - 4, f_) != record_len - 4) {
    err_ = {ParseError::TruncatedField, rec_off, "record body truncated"};
    return false;
  }
  ParseResult res = parse_record(raw.data(), raw.size(), out);
  if (!res.ok()) {
    res.error_offset = rec_off;
    err_ = res;
    return false;
  }
  out.record_offset = rec_off;
  offset_ += record_len;
  if (!is_known_time_base(out.time_base)) {
    err_ = {ParseError::UnknownTimeBase, rec_off, "unknown time base"};
    return false;
  }
  return true;
}

} // namespace relay
