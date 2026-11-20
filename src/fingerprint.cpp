#include "fingerprint.hpp"

#include <algorithm>
#include <cstring>

#include "sha256.hpp"

namespace mb {

namespace {
// FNV-1a 64-bit, deterministic and dependency-free.
u64 fnv1a(const u8* data, std::size_t len, u64 seed) {
  u64 h = 1469598103934665603ULL ^ seed;
  for (std::size_t i = 0; i < len; ++i) {
    h ^= data[i];
    h *= 1099511628211ULL;
  }
  return h;
}

constexpr std::size_t kFpWords = 8;  // 512-bit content fingerprint
}  // namespace

Fingerprint Fingerprinter::compute(const Segment& s, TrackKind kind) {
  Fingerprint fp;
  fp.sha256 = Sha256::hash(s.normalized);
  fp.bits.resize(kFpWords);
  // Sample the canonical byte stream at evenly spaced offsets; the stride
  // differs for video and audio so identical byte noise cannot collide.
  u64 stride_seed = kind == TrackKind::Video ? 0x9E3779B97F4A7C15ULL
                                             : 0xD1B54A32D192ED03ULL;
  std::size_t n = s.normalized.size();
  for (std::size_t w = 0; w < kFpWords; ++w) {
    u64 word = 0;
    for (std::size_t bit = 0; bit < 64u; ++bit) {
      std::size_t idx = (w * 64u + bit * 7u +
                         std::size_t(stride_seed % 97u)) %
                        (n + 1u);
      u8 byte = idx < n ? s.normalized[idx] : u8(w * 31u + bit);
      u8 probe = u8(fnv1a(&byte, 1, stride_seed ^ ((u64(w) << 8) | bit)) &
                    0xFFu);
      if ((probe ^ byte) & 0x01u) word |= (u64(1) << bit);
    }
    fp.bits[w] = word;
  }
  return fp;
}

double Fingerprinter::similarity(const Fingerprint& a, const Fingerprint& b) {
  std::size_t words = std::min(a.bits.size(), b.bits.size());
  if (words == 0) return 0.0;
  unsigned diff = 0;
  unsigned total = unsigned(words) * 64u;
  for (std::size_t i = 0; i < words; ++i) {
    diff += unsigned(__builtin_popcountll(a.bits[i] ^ b.bits[i]));
  }
  return 1.0 - double(diff) / double(total);
}

DedupResult Fingerprinter::dedup(
    const std::vector<Segment>& segments,
    const std::map<u32, TrackInfo>& tracks) {
  DedupResult out;
  std::vector<Fingerprint> fps;
  fps.reserve(segments.size());
  for (const auto& s : segments) {
    auto it = tracks.find(s.track_id);
    TrackKind kind = it == tracks.end() ? TrackKind::Video : it->second.kind;
    fps.push_back(compute(s, kind));
  }
  for (std::size_t i = 0; i < segments.size(); ++i) {
    for (std::size_t j = i + 1; j < segments.size(); ++j) {
      if (segments[i].track_id == segments[j].track_id) continue;
      auto ti = tracks.find(segments[i].track_id);
      auto tj = tracks.find(segments[j].track_id);
      bool drift_blocked =
          (ti != tracks.end() && ti->second.merge_blocked) ||
          (tj != tracks.end() && tj->second.merge_blocked);
      bool same_kind = ti != tracks.end() && tj != tracks.end() &&
                       ti->second.kind == tj->second.kind;
      if (!same_kind) continue;
      bool exact = fps[i].sha256 == fps[j].sha256;
      double sim = similarity(fps[i], fps[j]);
      if (exact) {
        out.exact_pairs++;
        DuplicateReport r;
        r.kept_segment = segments[i].segment_id;
        r.other_segment = segments[j].segment_id;
        r.other_track = segments[j].track_id;
        r.similarity = 1.0;
        r.blocked_by_drift = drift_blocked;
        r.auto_merged = !drift_blocked;
        if (r.auto_merged) out.merged_refs.push_back(segments[j].segment_id);
        out.reports.push_back(r);
      } else if (sim >= kFingerprintSimilarity) {
        out.candidate_pairs++;
        out.reports.push_back(
            {segments[i].segment_id, segments[j].segment_id,
             segments[j].track_id, sim, false, true, drift_blocked});
      }
    }
  }
  return out;
}

}  // namespace mb
