#include "report.h"

#include <cstdio>

namespace h264 {

namespace {

std::string json_escape(const std::string& s) {
  std::string out;
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
          snprintf(buf, sizeof(buf), "\\u%04x", c);
          out += buf;
        } else {
          out += c;
        }
    }
  }
  return out;
}

std::string num(double v) {
  char buf[32];
  snprintf(buf, sizeof(buf), "%.10g", v);
  return buf;
}

std::string u64(uint64_t v) { return std::to_string(v); }

}  // namespace

std::string report_to_json(const Report& r) {
  const StreamStats& s = r.stream;
  std::string j = "{\n";
  j += "  \"version\": 1,\n";
  j += "  \"file\": \"" + json_escape(r.file) + "\",\n";
  j += "  \"size_bytes\": " + u64(r.size_bytes) + ",\n";
  j += "  \"sps\": {\"profile_idc\": " + u64(r.sps.profile_idc) +
       ", \"level_idc\": " + u64(r.sps.level_idc) +
       ", \"width\": " + u64(r.sps.width) +
       ", \"height\": " + u64(r.sps.height) +
       ", \"chroma_format_idc\": " + u64(r.sps.chroma_format_idc) +
       ", \"frame_mbs_only_flag\": " + (r.sps.frame_mbs_only_flag ? "true" : "false") +
       ", \"mbaff\": " + (r.sps.mbaff ? "true" : "false") +
       ", \"fps\": " + num(r.sps.fps) + "},\n";
  j += "  \"counts\": {\"I\": " + u64(s.count_i) + ", \"P\": " + u64(s.count_p) +
       ", \"B\": " + u64(s.count_b) + ", \"other\": " + u64(s.count_other) +
       ", \"total\": " + u64(s.total_frames()) + "},\n";
  j += "  \"gops\": [";
  for (size_t i = 0; i < s.gops.size(); ++i) {
    const GopInfo& g = s.gops[i];
    j += (i == 0 ? "\n" : ",\n");
    j += "    {\"index\": " + u64(g.index) +
         ", \"start_byte_offset\": " + u64(g.start_byte_offset) +
         ", \"au_index\": " + u64(g.au_index) +
         ", \"frame_count\": " + u64(g.frame_count) + ", \"type_mix\": {";
    bool first = true;
    for (const auto& kv : g.type_mix) {
      j += (first ? "\"" : ", \"") + kv.first + "\": " + u64(kv.second);
      first = false;
    }
    j += "}, \"poc_start\": " + std::to_string(g.poc_start) + "}";
  }
  j += s.gops.empty() ? "],\n" : "\n  ],\n";
  j += "  \"slices\": {\"per_picture_histogram\": {";
  bool first = true;
  for (const auto& kv : s.slices_per_picture) {
    j += (first ? "\"" : ", \"") + std::to_string(kv.first) + "\": " + u64(kv.second);
    first = false;
  }
  double ue_avg = s.ue_count ? static_cast<double>(s.ue_bits) / s.ue_count : 0.0;
  j += "}, \"ue_avg_bits\": " + num(ue_avg) +
       ", \"header_bits_total\": " + u64(s.header_bits) + "},\n";
  j += "  \"warnings\": [";
  for (size_t i = 0; i < s.warnings.size(); ++i) {
    j += (i == 0 ? "" : ", ") ;
    j += "\"" + json_escape(s.warnings[i]) + "\"";
  }
  j += "]";
  if (s.warning_overflow) {
    j += ",\n  \"warning_overflow\": " + u64(s.warning_overflow);
  }
  if (r.has_stats) {
    j += ",\n  \"stats\": {\"elapsed_sec\": " + num(r.elapsed_sec) +
         ", \"peak_rss_mb\": " + num(r.peak_rss_mb) + "}";
  }
  j += "\n}\n";
  return j;
}

std::string report_to_text(const Report& r) {
  const StreamStats& s = r.stream;
  std::string t;
  t += "file: " + r.file + " (" + u64(r.size_bytes) + " bytes)\n";
  t += "sps: profile=" + u64(r.sps.profile_idc) + " level=" + u64(r.sps.level_idc) +
       " " + u64(r.sps.width) + "x" + u64(r.sps.height) +
       " chroma=" + u64(r.sps.chroma_format_idc) +
       " frame_mbs_only=" + (r.sps.frame_mbs_only_flag ? "1" : "0") +
       " mbaff=" + (r.sps.mbaff ? "1" : "0") + " fps=" + num(r.sps.fps) + "\n";
  t += "counts: I=" + u64(s.count_i) + " P=" + u64(s.count_p) +
       " B=" + u64(s.count_b) + " other=" + u64(s.count_other) +
       " total=" + u64(s.total_frames()) + "\n";
  for (const GopInfo& g : s.gops) {
    t += "gop " + u64(g.index) + ": off=" + u64(g.start_byte_offset) +
         " au=" + u64(g.au_index) + " frames=" + u64(g.frame_count) + " mix=";
    bool first = true;
    for (const auto& kv : g.type_mix) {
      t += (first ? "" : ",") + kv.first + ":" + u64(kv.second);
      first = false;
    }
    t += " poc=" + std::to_string(g.poc_start) + "\n";
  }
  t += "slices: hist=";
  bool first = true;
  for (const auto& kv : s.slices_per_picture) {
    t += (first ? "" : ",") + std::to_string(kv.first) + ":" + u64(kv.second);
    first = false;
  }
  double ue_avg = s.ue_count ? static_cast<double>(s.ue_bits) / s.ue_count : 0.0;
  t += " ue_avg_bits=" + num(ue_avg) + " header_bits=" + u64(s.header_bits) + "\n";
  for (const std::string& w : s.warnings) {
    t += "warning: " + w + "\n";
  }
  if (s.warning_overflow) {
    t += "warning: (" + u64(s.warning_overflow) + " more suppressed)\n";
  }
  if (r.has_stats) {
    t += "stats: elapsed=" + num(r.elapsed_sec) + "s peak_rss=" +
         num(r.peak_rss_mb) + "MB\n";
  }
  return t;
}

}  // namespace h264
