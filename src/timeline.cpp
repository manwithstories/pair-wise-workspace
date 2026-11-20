#include "timeline.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <sstream>

namespace mb {

u64 Timeline::ntp_to_unix_ns(u32 seconds, u32 fraction) {
  constexpr u64 kNtpOffset = 2208988800ULL;  // 1900-01-01 -> 1970-01-01
  u64 secs = u64(seconds) - kNtpOffset;
  u64 frac_ns = (u64(fraction) * u64(kNsPerSec)) >> 32;
  return secs * u64(kNsPerSec) + frac_ns;
}

Timeline::Timeline(std::vector<TrackInfo> tracks)
    : tracks_(std::move(tracks)) {
  for (const auto& t : tracks_) {
    TimelineStats s;
    s.ssrc = t.ssrc;
    stats_[t.ssrc] = s;
  }
}

const TrackInfo* Timeline::find(u32 ssrc) const {
  for (const auto& t : tracks_)
    if (t.ssrc == ssrc) return &t;
  return nullptr;
}

void Timeline::add_sr(const RawRecord& rec) {
  if (rec.body.size() < sizeof(RtcpSrRecord)) return;
  RtcpSrRecord sr{};
  std::memcpy(&sr, rec.body.data(), sizeof(sr));
  Anchor a{ntp_to_unix_ns(sr.ntp_seconds, sr.ntp_fraction), sr.rtp_timestamp};
  anchors_[sr.ssrc].push_back(a);
  auto& s = stats_[sr.ssrc];
  s.sr_count++;
  s.estimated = false;
  if (origin_ns_ == 0 || i64(a.ntp_ns) < origin_ns_)
    origin_ns_ = i64(a.ntp_ns);
}

i64 Timeline::map_one(const TrackInfo& t, u32 rtp_ts,
                      i64& uncertainty) const {
  uncertainty = 0;
  auto it = anchors_.find(t.ssrc);
  if (it != anchors_.end() && !it->second.empty()) {
    const auto& aa = it->second;
    // Last anchor at or before this timestamp (wrap-aware modular compare).
    const Anchor* chosen = &aa.front();
    for (const auto& a : aa) {
      if (tsDelta(rtp_ts, a.rtp_ts) >= 0) chosen = &a;
    }
    i64 ticks = tsDelta(rtp_ts, chosen->rtp_ts);
    i64 ns = i64(chosen->ntp_ns) +
             (ticks * kNsPerSec) / i64(t.clock_rate);
    // Drift residual between two SRs bounds the interpolation error.
    uncertainty = (aa.size() > 1) ? 2'000'000 : 250'000;  // 2 ms / 0.25 ms
    return ns;
  }
  // No SR: estimate from the first observed packet at timeline origin.
  auto base = estimate_base_.find(t.ssrc);
  if (base == estimate_base_.end()) {
    uncertainty = 50'000'000;  // 50 ms upper bound without any report
    return origin_ns_ + i64((u64(rtp_ts) * u64(kNsPerSec)) /
                            u64(t.clock_rate));
  }
  i64 ticks = tsDelta(rtp_ts, base->second.first);
  uncertainty = 50'000'000;
  return i64(base->second.second) +
         (ticks * kNsPerSec) / i64(t.clock_rate);
}

bool Timeline::map_packets(std::vector<PacketRecord>& packets,
                           std::vector<ErrorRecord>& errors) {
  std::map<u32, i64> last_ns;
  std::map<u32, u32> first_rtp;
  bool ok = true;
  for (auto& p : packets) {
    const TrackInfo* t = find(p.ssrc);
    if (!t) {
      errors.push_back({0, p.byte_offset, p.ssrc, p.seq, "timeline.unknown_ssrc",
                        "no track declaration for SSRC",
                        Recoverability::Unrecoverable});
      continue;
    }
    if (!first_rtp.contains(p.ssrc)) {
      first_rtp[p.ssrc] = p.rtp_ts;
      if (!anchors_.contains(p.ssrc))
        estimate_base_[p.ssrc] = {p.rtp_ts, u64(origin_ns_)};
    }
    i64 unc = 0;
    i64 ns = map_one(*t, p.rtp_ts, unc);
    if (last_ns.contains(p.ssrc)) {
      i64 jump = ns - last_ns[p.ssrc];
      if (jump < -kMaxTsBackJumpNs) {
        errors.push_back({t->track_id, p.byte_offset, p.ssrc, p.seq,
                          "timeline.illegal_ts",
                          "timestamp back-jump exceeds legal bound",
                          Recoverability::Recovered});
        p.recovery = Recoverability::Recovered;
        ok = false;
      }
    }
    p.mapped_ns = ns;
    p.uncertainty_ns = unc;
    last_ns[p.ssrc] = ns;
    auto& s = stats_[p.ssrc];
    s.max_uncertainty_ns = std::max(s.max_uncertainty_ns, unc);
    if (!anchors_.contains(p.ssrc)) s.estimated = true;
  }
  return ok;
}

void Timeline::evaluate_drift() {
  // Compare each track's average RTP-clock advance against its SR wall-time
  // advance. Tracks without two SRs inherit the generator-declared drift.
  for (auto& t : tracks_) {
    auto& s = stats_[t.ssrc];
    auto it = anchors_.find(t.ssrc);
    if (it != anchors_.end() && it->second.size() >= 2) {
      const auto& a = it->second;
      i64 rtp_d = tsDelta(a.back().rtp_ts, a.front().rtp_ts);
      i64 wall_d = i64(a.back().ntp_ns) - i64(a.front().ntp_ns);
      i64 expected_ns = (rtp_d * kNsPerSec) / i64(t.clock_rate);
      if (wall_d > 0) {
        s.drift_ppm = double(expected_ns - wall_d) * 1e6 / double(wall_d);
      }
    } else {
      s.drift_ppm = t.drift_ppm;
    }
    t.drift_ppm = s.drift_ppm;
    bool blocked = std::fabs(s.drift_ppm) > kDriftLimitPpm || s.estimated;
    t.drift_over_limit = std::fabs(s.drift_ppm) > kDriftLimitPpm;
    t.merge_blocked = blocked;
    s.merge_blocked = blocked;
  }
}

std::string json_escape(std::string_view s) {
  std::string out;
  out.reserve(s.size() + 2);
  for (char c : s) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (static_cast<unsigned char>(c) < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof(buf), "\\u%04x",
                        static_cast<unsigned char>(c));
          out += buf;
        } else {
          out.push_back(c);
        }
    }
  }
  return out;
}

void JsonWriter::sep() {
  if (filled_.empty()) return;
  if (filled_.back()) text_ += ",";
  filled_.back() = true;
}
void JsonWriter::key(std::string_view k) {
  sep();
  text_ += "\"" + json_escape(k) + "\":";
  filled_.back() = false;  // value follows; no comma before it
}
void JsonWriter::value_string(std::string_view v) {
  sep();
  text_ += "\"" + json_escape(v) + "\"";
}
void JsonWriter::value_int(long long v) {
  sep();
  text_ += std::to_string(v);
}
void JsonWriter::value_double(double v) {
  sep();
  std::ostringstream os;
  os.precision(6);
  os << v;
  text_ += os.str();
}
void JsonWriter::value_bool(bool v) {
  sep();
  text_ += v ? "true" : "false";
}
void JsonWriter::begin_object() {
  sep();
  text_ += "{";
  stack_.push_back('{');
  filled_.push_back(false);
}
void JsonWriter::end_object() {
  text_ += "}";
  stack_.pop_back();
  filled_.pop_back();
  if (!filled_.empty()) filled_.back() = true;
}
void JsonWriter::begin_array() {
  sep();
  text_ += "[";
  stack_.push_back('[');
  filled_.push_back(false);
}
void JsonWriter::end_array() {
  text_ += "]";
  stack_.pop_back();
  filled_.pop_back();
  if (!filled_.empty()) filled_.back() = true;
}

}  // namespace mb
