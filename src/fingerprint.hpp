#pragma once
// Content fingerprints and cross-camera deduplication. Every segment gets a
// canonical byte SHA-256 and a coarse content fingerprint. Identical hashes
// merge into a shared reference automatically; different hashes with close
// fingerprints are reported as candidates only and never deleted.
#include <array>
#include <string>
#include <vector>

#include "common.hpp"
#include "segmenter.hpp"

namespace mb {

struct Fingerprint {
  std::array<u8, 32> sha256{};
  std::vector<u64> bits;   // content fingerprint words
};

struct DuplicateReport {
  u32 kept_segment = 0;            // canonical reference segment id
  u32 other_segment = 0;
  u32 other_track = 0;
  double similarity = 0.0;
  bool auto_merged = false;        // exact hash + drift allowed
  bool candidate_only = false;     // similar fingerprint, different hash
  bool blocked_by_drift = false;   // identical content, merge forbidden
};

struct DedupResult {
  std::vector<DuplicateReport> reports;
  std::vector<u32> merged_refs;    // segment ids that reference another
  u64 exact_pairs = 0;
  u64 candidate_pairs = 0;
};

class Fingerprinter {
 public:
  // Canonical bytes are produced by the segmenter (rtp ts + payload).
  static Fingerprint compute(const Segment& s, TrackKind kind);
  static double similarity(const Fingerprint& a, const Fingerprint& b);

  // Compare segments across tracks. Track drift flags gate auto-merge.
  static DedupResult dedup(const std::vector<Segment>& segments,
                           const std::map<u32, TrackInfo>& tracks);
};

}  // namespace mb
