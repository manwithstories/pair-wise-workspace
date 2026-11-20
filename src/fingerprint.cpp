#include "fingerprint.hpp"

#include <algorithm>
#include <cstring>

#include "sha256.hpp"

namespace mb {

namespace {
constexpr std::size_t kFpWords = 8;  // 1 size word + 7 block digest words
}  // namespace

Fingerprint Fingerprinter::compute(const Segment& s, TrackKind kind) {
  Fingerprint fp;
  fp.sha256 = Sha256::hash(s.normalized);
  fp.bits.resize(kFpWords);
  u64 kind_seed = kind == TrackKind::Video ? 0x9E3779B97F4A7C15ULL
                                            : 0xD1B54A32D192ED03ULL;
  // Leading word: canonical size.
  fp.bits[0] = u64(s.normalized.size()) * 1099511628211ULL ^ kind_seed;
  // Block FNV-1a digests over equal-sized canonical chunks. Exact content
  // matches block-for-block; a localised edit changes only the block(s) it
  // lands in, so the block-equality ratio is a stable closeness measure and
  // the SHA-256 above remains the authoritative exact-match test.
  constexpr std::size_t kBlocks = kFpWords - 1;
  std::size_t n = s.normalized.size();
  for (std::size_t blk = 0; blk < kBlocks; ++blk) {
    std::size_t begin = n * blk / kBlocks;
    std::size_t end = n * (blk + 1) / kBlocks;
    u64 h = 1469598103934665603ULL ^ kind_seed ^ (u64(blk) * 40503ULL);
    for (std::size_t i = begin; i < end; ++i) {
      h ^= s.normalized[i];
      h *= 1099511628211ULL;
    }
    fp.bits[1 + blk] = h;
  }
  return fp;
}

double Fingerprinter::similarity(const Fingerprint& a, const Fingerprint& b) {
  std::size_t words = std::min(a.bits.size(), b.bits.size());
  if (words == 0) return 0.0;
  // Fraction of fingerprint words (size word + block digests) that match.
  // Localised edits change few blocks; unrelated content changes nearly all.
  std::size_t equal = 0;
  for (std::size_t i = 0; i < words; ++i)
    if (a.bits[i] == b.bits[i]) ++equal;
  return double(equal) / double(words);
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
