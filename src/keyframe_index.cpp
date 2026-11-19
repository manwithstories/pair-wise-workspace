#include <algorithm>
#include <cstring>
#include <fstream>
#include <unordered_map>

#include "media_archive/keyframe_index.h"
#include "media_archive/packet_reader.h"

namespace media_archive {
namespace {
constexpr char kMagic[8] = {'M', 'A', 'K', 'I', 'D', 'X', '1', 0};

void put_u32(std::ofstream& f, uint32_t v) {
  char b[4];
  for (int i = 0; i < 4; ++i) b[i] = static_cast<char>((v >> (8 * i)) & 0xFF);
  f.write(b, 4);
}
void put_u64(std::ofstream& f, uint64_t v) {
  char b[8];
  for (int i = 0; i < 8; ++i) b[i] = static_cast<char>((v >> (8 * i)) & 0xFF);
  f.write(b, 8);
}
bool get_u32(std::ifstream& f, uint32_t* v) {
  unsigned char b[4];
  if (!f.read(reinterpret_cast<char*>(b), 4)) return false;
  *v = 0;
  for (int i = 0; i < 4; ++i) *v |= uint32_t(b[i]) << (8 * i);
  return true;
}
bool get_u64(std::ifstream& f, uint64_t* v) {
  unsigned char b[8];
  if (!f.read(reinterpret_cast<char*>(b), 8)) return false;
  *v = 0;
  for (int i = 0; i < 8; ++i) *v |= uint64_t(b[i]) << (8 * i);
  return true;
}
}  // namespace

std::vector<KeyframeEntry> build_keyframe_index(
    const std::vector<ParsedFrame>& frames,
    const std::vector<RepairedPacket>& repaired,
    const std::vector<uint8_t>& stream,
    uint64_t stream_size) {
  (void)stream_size;
  // Index repaired packets by source byte offset for O(1) frame matching.
  std::unordered_map<int64_t, size_t> by_offset;
  by_offset.reserve(repaired.size() * 2);
  for (size_t i = 0; i < repaired.size(); ++i)
    by_offset.emplace(repaired[i].packet.source_offset, i);

  std::vector<KeyframeEntry> entries;
  for (const ParsedFrame& frame : frames) {
    if (!frame.keyframe) continue;
    auto found = by_offset.find(static_cast<int64_t>(frame.source_offset));
    if (found == by_offset.end()) continue;
    const RepairedPacket& rp = repaired[found->second];
    KeyframeEntry e;
    e.pts_ticks = rp.repaired_pts;
    e.source_offset = frame.source_offset;
    e.size_bytes = frame.size_bytes;
    e.frame_type = frame.frame_type;
    e.extended_seq = rp.packet.seq;
    const uint64_t end = frame.source_offset + frame.size_bytes;
    if (end <= stream.size()) {
      e.checksum =
          crc32_ieee(stream.data() + frame.source_offset, frame.size_bytes);
    }
    entries.push_back(e);
  }
  std::sort(entries.begin(), entries.end(),
            [](const KeyframeEntry& a, const KeyframeEntry& b) {
              return a.pts_ticks < b.pts_ticks;
            });
  return entries;
}

SeekCheck verify_seek(const std::vector<KeyframeEntry>& entries,
                      uint64_t requested_pts) {
  SeekCheck check;
  check.requested_pts = requested_pts;
  if (entries.empty()) return check;
  // Manual binary search for the last keyframe with pts_ticks <= request,
  // avoiding comparator-argument-order ambiguity across STL implementations.
  size_t lo = 0;
  size_t hi = entries.size();  // exclusive upper bound
  while (lo < hi) {
    const size_t mid = lo + (hi - lo) / 2;
    if (entries[mid].pts_ticks <= requested_pts)
      lo = mid + 1;
    else
      hi = mid;
  }
  auto it = lo == 0 ? entries.begin() : entries.begin() + (lo - 1);
  check.landed_pts = it->pts_ticks;
  check.source_offset = it->source_offset;
  check.error_ms = ticks_to_ms(
      requested_pts >= it->pts_ticks ? requested_pts - it->pts_ticks
                                     : it->pts_ticks - requested_pts);
  return check;
}

Diagnostic write_keyframe_index(const std::string& path, CodecType codec,
                                const std::vector<KeyframeEntry>& entries,
                                uint64_t stream_size) {
  std::ofstream f(path, std::ios::binary | std::ios::trunc);
  if (!f) return diag(Status::IoError, "cannot open index output: " + path);
  uint32_t running_crc = 0;
  f.write(kMagic, 8);
  put_u32(f, static_cast<uint32_t>(codec));
  put_u32(f, static_cast<uint32_t>(entries.size()));
  put_u64(f, stream_size);
  put_u32(f, 0);  // checksum placeholder
  for (const auto& e : entries) {
    put_u64(f, e.pts_ticks);
    put_u64(f, e.source_offset);
    put_u32(f, e.size_bytes);
    put_u32(f, e.frame_type);
    put_u32(f, e.extended_seq);
    put_u32(f, e.checksum);
    running_crc = crc32_ieee(reinterpret_cast<const uint8_t*>(&e), sizeof(e)) ^
                  (running_crc ^ 0u);
  }
  f.seekp(8 + 4 + 4 + 8);
  put_u32(f, running_crc);
  if (!f) return diag(Status::IoError, "index write failed: " + path);
  return {};
}

Diagnostic read_keyframe_index(const std::string& path, CodecType expected,
                               std::vector<KeyframeEntry>* entries,
                               IndexFileHeader* header_out) {
  std::ifstream f(path, std::ios::binary);
  if (!f) return diag(Status::UsageError, "cannot open index: " + path);
  IndexFileHeader hdr{};
  if (!f.read(hdr.magic, 8) || std::memcmp(hdr.magic, kMagic, 8) != 0)
    return diag(Status::MediaCorrupt, "bad index magic", 0);
  if (!get_u32(f, &hdr.codec) || !get_u32(f, &hdr.record_count) ||
      !get_u64(f, &hdr.stream_size) || !get_u32(f, &hdr.checksum))
    return diag(Status::MediaCorrupt, "truncated index header",
                static_cast<int64_t>(f.tellg()));
  if (static_cast<CodecType>(hdr.codec) != expected)
    return diag(Status::MediaCorrupt, "index codec tag mismatch", 8);
  if (hdr.record_count > kMaxKeyframesPerBlock * 1024)
    return diag(Status::UsageError, "index record count exceeds limit", 12);

  entries->clear();
  entries->reserve(hdr.record_count);
  uint32_t running_crc = 0;
  for (uint32_t i = 0; i < hdr.record_count; ++i) {
    KeyframeEntry e;
    const int64_t pos = static_cast<int64_t>(f.tellg());
    if (!get_u64(f, &e.pts_ticks) || !get_u64(f, &e.source_offset) ||
        !get_u32(f, &e.size_bytes) || !get_u32(f, &e.frame_type) ||
        !get_u32(f, &e.extended_seq) || !get_u32(f, &e.checksum))
      return diag(Status::MediaCorrupt, "truncated index record", pos);
    running_crc =
        crc32_ieee(reinterpret_cast<const uint8_t*>(&e), sizeof(e)) ^
        running_crc;
    entries->push_back(e);
  }
  if (running_crc != hdr.checksum)
    return diag(Status::MediaCorrupt,
                "index record checksum mismatch", 24);
  if (header_out) *header_out = hdr;
  return {};
}

}  // namespace media_archive
