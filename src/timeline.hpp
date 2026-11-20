#pragma once
// Unified nanosecond timeline. RTCP Sender Reports anchor each track's RTP
// clock to wall time; without an SR the clock is estimated from neighbouring
// packets and an uncertainty upper bound is attached to every mapping.
#include <cstdint>
#include <map>
#include <optional>
#include <string>
#include <vector>

#include "capture.hpp"
#include "common.hpp"

namespace mb {

struct SenderReport {
  u32 ssrc = 0;
  u64 ntp_ns = 0;        // NTP timestamp converted to Unix-epoch nanoseconds
  u32 rtp_timestamp = 0;
};

struct TimelineStats {
  u32 ssrc = 0;
  u64 sr_count = 0;
  bool estimated = false;
  i64 max_uncertainty_ns = 0;
  double drift_ppm = 0.0;
  bool merge_blocked = false;
};

class Timeline {
 public:
  explicit Timeline(std::vector<TrackInfo> tracks);

  // Import RTCP SR records (raw container records) for one SSRC.
  void add_sr(const RawRecord& rec);

  // Map every packet's RTP timestamp onto the shared nanosecond timeline.
  // Returns false if a packet carries an illegal out-of-range timestamp.
  bool map_packets(std::vector<PacketRecord>& packets,
                   std::vector<ErrorRecord>& errors);

  // Compute cross-track drift against every other track and flag tracks whose
  // drift exceeds kDriftLimitPpm: they stay in the output but silent cross-track
  // merging is forbidden.
  void evaluate_drift();

  const std::map<u32, TimelineStats>& stats() const { return stats_; }
  const std::vector<TrackInfo>& tracks() const { return tracks_; }
  std::vector<TrackInfo>& mutable_tracks() { return tracks_; }
  i64 origin_ns() const { return origin_ns_; }

  static u64 ntp_to_unix_ns(u32 seconds, u32 fraction);

 private:
  struct Anchor {
    u64 ntp_ns;
    u32 rtp_ts;
  };
  const TrackInfo* find(u32 ssrc) const;
  i64 map_one(const TrackInfo& t, u32 rtp_ts, i64& uncertainty) const;

  std::vector<TrackInfo> tracks_;
  std::map<u32, std::vector<Anchor>> anchors_;
  std::map<u32, TimelineStats> stats_;
  std::map<u32, std::pair<u32, u64>> estimate_base_;  // ssrc -> (rtp, ns)
  i64 origin_ns_ = 0;
};

// Minimal deterministic JSON writer (string escaping included).
class JsonWriter {
 public:
  JsonWriter() { text_ = "{"; }
  void key(std::string_view k);
  void value_string(std::string_view v);
  void value_int(long long v);
  void value_double(double v);
  void value_bool(bool v);
  void begin_object();
  void end_object();
  void begin_array();
  void end_array();
  std::string finish() { text_ += "}"; return text_; }

 private:
  void sep();
  std::string text_;
  std::vector<char> stack_;  // '{' or '['
  std::vector<bool> filled_; // whether the current container has an entry
};

std::string json_escape(std::string_view s);

}  // namespace mb
