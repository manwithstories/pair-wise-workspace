#pragma once
// Shared value types, limits and error model for the media batch pipeline.
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace mb {

using i64 = std::int64_t;
using u64 = std::uint64_t;
using u32 = std::uint32_t;
using u16 = std::uint16_t;
using u8 = std::uint8_t;

inline constexpr i64 kNsPerSec = 1'000'000'000LL;
inline constexpr int kRtpVersion = 2;
inline constexpr std::size_t kMaxPacketBytes = 2048; // reject oversized packets
inline constexpr u32 kReorderWindow = 64;            // jitter buffer depth
inline constexpr u32 kSeqMod = 0x10000u;             // RTP sequence modulus
inline constexpr double kDriftLimitPpm = 1000.0;     // cross-track merge limit
inline constexpr i64 kSegmentTargetNs = 1'000'000'000LL;
inline constexpr i64 kMinSegmentNs = 250'000'000LL;
inline constexpr i64 kKeyframeToleranceNs = 250'000'000LL;
inline constexpr i64 kSilenceWindowNs = 20'000'000LL;  // 20 ms RMS window
inline constexpr i64 kMinSilenceNs = 200'000'000LL;    // do not report shorter
inline constexpr double kSilenceRmsThreshold = 0.005;  // ~ -46 dBFS
inline constexpr double kFingerprintSimilarity = 0.85;
inline constexpr i64 kMaxTsBackJumpNs = 60 * kNsPerSec;

enum class TrackKind : u8 { Video = 0, Audio = 1 };
enum class Recoverability : u8 { Ok, Recovered, Unrecoverable };
enum class TaskStatus : u8 { Pending, Ready, Running, Done, Skipped, Failed };

struct ErrorRecord {
  u32 track_id = 0;
  u64 byte_offset = 0;
  std::optional<u32> ssrc;
  std::optional<u32> seq;
  std::string code;
  std::string message;
  Recoverability recovery = Recoverability::Ok;
};

struct PacketRecord {
  u32 ssrc = 0;
  u32 seq = 0;          // raw 16-bit RTP sequence number
  u64 abs_seq = 0;      // extended sequence number (wrap-aware)
  u32 rtp_ts = 0;
  u8 payload_type = 0;
  bool marker = false;
  bool keyframe = false;
  std::vector<u8> payload;
  u64 byte_offset = 0;  // byte offset inside the capture file
  i64 mapped_ns = 0;    // unified timeline position (filled by timeline)
  i64 uncertainty_ns = 0;
  Recoverability recovery = Recoverability::Ok;
  bool corrupt = false;
};

struct TrackInfo {
  u32 track_id = 0;
  u32 ssrc = 0;
  TrackKind kind = TrackKind::Video;
  u32 clock_rate = 90000;
  u16 channels = 0;
  std::string name;
  double drift_ppm = 0.0;
  bool drift_over_limit = false;
  bool merge_blocked = false;
};

// Little-endian byte helpers (capture files are LE on disk).
inline u16 rd16(const u8* p) {
  return static_cast<u16>(u16(p[0]) | (u16(p[1]) << 8));
}
inline u32 rd32(const u8* p) {
  return u32(p[0]) | (u32(p[1]) << 8) | (u32(p[2]) << 16) | (u32(p[3]) << 24);
}
inline void wr16(std::vector<u8>& b, u16 v) {
  b.push_back(u8(v));
  b.push_back(u8(v >> 8));
}
inline void wr32(std::vector<u8>& b, u32 v) {
  b.push_back(u8(v));
  b.push_back(u8(v >> 8));
  b.push_back(u8(v >> 16));
  b.push_back(u8(v >> 24));
}

// Signed modular difference for wrapping 16/32-bit RTP counters.
inline i64 seqDelta(u32 a, u32 b) {  // a - b, wrapped into (-32768, 32767]
  i64 d = i64(a) - i64(b);
  if (d > 32767) d -= 65536;
  if (d <= -32768) d += 65536;
  return d;
}
inline i64 tsDelta(u32 a, u32 b) {  // a - b, wrapped into (-2^31, 2^31]
  i64 d = i64(a) - i64(b);
  if (d > INT64_C(0x7fffffff)) d -= INT64_C(0x100000000);
  if (d <= -INT64_C(0x80000000)) d += INT64_C(0x100000000);
  return d;
}

}  // namespace mb
