// report_writer.cpp — 手写 JSON 序列化/解析与固定键序终端摘要
#include "report_writer.h"

#include <algorithm>
#include <cctype>
#include <cstdio>
#include <fstream>
#include <sstream>

namespace popfix {

namespace {

std::string json_escape(const std::string& s) {
  std::string out;
  for (char c : s) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\t': out += "\\t"; break;
      default: out.push_back(c);
    }
  }
  return out;
}

std::string fmt_double(double v) {
  char buf[64];
  std::snprintf(buf, sizeof(buf), "%.9g", v);
  return std::string(buf);
}

// ---- 极简 JSON 读取器：只服务本工具自身写出的报告 --------------------
class MiniJson {
 public:
  explicit MiniJson(const std::string& s) : s_(s) {}
  void skip_ws() {
    while (i_ < s_.size() &&
           (s_[i_] == ' ' || s_[i_] == '\n' || s_[i_] == '\t' ||
            s_[i_] == '\r'))
      ++i_;
  }
  bool consume(char c) {
    skip_ws();
    if (i_ < s_.size() && s_[i_] == c) {
      ++i_;
      return true;
    }
    return false;
  }
  void expect(char c) {
    if (!consume(c))
      throw WavIOError(std::string("报告 JSON 解析失败，缺少 '") + c + "'",
                       false);
  }
  std::string parse_string() {
    expect('"');
    std::string out;
    while (i_ < s_.size() && s_[i_] != '"') {
      if (s_[i_] == '\\' && i_ + 1 < s_.size()) {
        out.push_back(s_[i_ + 1]);
        i_ += 2;
      } else {
        out.push_back(s_[i_++]);
      }
    }
    expect('"');
    return out;
  }
  double parse_number() {
    skip_ws();
    std::size_t start = i_;
    if (i_ < s_.size() && (s_[i_] == '-' || s_[i_] == '+')) ++i_;
    while (i_ < s_.size() &&
           (std::isdigit(static_cast<unsigned char>(s_[i_])) ||
            s_[i_] == '.' || s_[i_] == 'e' || s_[i_] == 'E' ||
            s_[i_] == '+' || s_[i_] == '-'))
      ++i_;
    return std::stod(s_.substr(start, i_ - start));
  }
  // 在当前对象内查找键；未找到返回 false（调用方决定是否必需）。
  bool find_key(const std::string& key) {
    skip_ws();
    while (i_ < s_.size() && s_[i_] != '}') {
      const std::string k = parse_string();
      expect(':');
      if (k == key) return true;
      skip_value();
      skip_ws();
      if (!consume(',')) break;
    }
    return false;
  }
  void skip_value() {
    skip_ws();
    if (i_ >= s_.size()) return;
    if (s_[i_] == '"') {
      parse_string();
    } else if (s_[i_] == '{' || s_[i_] == '[') {
      const char open = s_[i_];
      const char close = open == '{' ? '}' : ']';
      int depth = 0;
      do {
        if (s_[i_] == open) ++depth;
        if (s_[i_] == close) --depth;
        ++i_;
      } while (i_ < s_.size() && depth > 0);
    } else {
      parse_number();
    }
  }
  std::size_t pos() const { return i_; }
  void seek(std::size_t p) { i_ = p; }

 private:
  const std::string& s_;
  std::size_t i_ = 0;
};

}  // namespace

void write_report(const std::string& path, const RepairResult& result,
                  const DetectorConfig& dcfg, const RepairConfig& rcfg,
                  bool dry_run) {
  validate_output_path(path);
  std::ostringstream os;
  const RepairStats& st = result.stats;
  os << "{\n";
  os << "  \"schema\": \"popfix-report/1\",\n";
  os << "  \"dry_run\": " << (dry_run ? "true" : "false") << ",\n";
  os << "  \"sample_rate\": " << result.source_rate << ",\n";
  os << "  \"channels\": " << result.source_channels << ",\n";
  os << "  \"frames\": " << result.source_frames << ",\n";
  os << "  \"threshold\": " << fmt_double(dcfg.threshold) << ",\n";
  os << "  \"context\": " << rcfg.context << ",\n";
  os << "  \"success\": " << (result.success ? "true" : "false") << ",\n";
  os << "  \"stats\": {\n";
  os << "    \"events\": " << st.event_count << ",\n";
  os << "    \"repaired_frames\": " << st.repaired_frames << ",\n";
  os << "    \"repaired_samples\": " << st.repaired_samples << ",\n";
  os << "    \"peak_before\": " << fmt_double(st.peak_before) << ",\n";
  os << "    \"peak_after\": " << fmt_double(st.peak_after) << ",\n";
  os << "    \"residual_energy\": " << fmt_double(st.residual_energy) << "\n";
  os << "  },\n";

  os << "  \"events\": [\n";
  // 事件按 event_id 稳定排序写出。
  std::vector<EventRepair> repairs = result.repairs;
  std::sort(repairs.begin(), repairs.end(),
            [](const EventRepair& a, const EventRepair& b) {
              return a.event_id < b.event_id;
            });
  for (std::size_t i = 0; i < repairs.size(); ++i) {
    const EventRepair& r = repairs[i];
    const ClipEvent& e = result.events[r.event_id];
    os << "    {\n";
    os << "      \"event_id\": " << r.event_id << ",\n";
    os << "      \"start_frame\": " << e.start_frame << ",\n";
    os << "      \"end_frame\": " << e.end_frame << ",\n";
    os << "      \"channel_mask\": "
       << static_cast<unsigned>(r.channel_mask) << ",\n";
    os << "      \"repair_start\": " << r.repair_start << ",\n";
    os << "      \"repair_end\": " << r.repair_end << ",\n";
    os << "      \"clipped_context\": "
       << (r.clipped_context ? "true" : "false") << ",\n";
    os << "      \"residual_energy\": "
       << fmt_double(r.residual_energy) << ",\n";
    os << "      \"failed\": " << (r.failed ? "true" : "false") << "\n";
    os << "    }" << (i + 1 < repairs.size() ? "," : "") << "\n";
  }
  os << "  ],\n";

  os << "  \"warnings\": [\n";
  for (std::size_t i = 0; i < result.warnings.size(); ++i) {
    const RepairWarning& w = result.warnings[i];
    os << "    {\"event_id\": " << w.event_id
       << ", \"channel\": " << static_cast<unsigned>(w.channel)
       << ", \"code\": \"" << json_escape(w.code)
       << "\", \"detail\": \"" << json_escape(w.detail) << "\"}"
       << (i + 1 < result.warnings.size() ? "," : "") << "\n";
  }
  os << "  ]\n}\n";

  const std::string tmp = path + ".tmp";
  ensure_parent_dir(path);
  std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
  if (!f) throw WavIOError("无法创建报告临时文件: " + tmp, true);
  f << os.str();
  if (!f) {
    f.close();
    std::remove(tmp.c_str());
    throw WavIOError("写报告失败: " + tmp, true);
  }
  f.close();
  if (std::rename(tmp.c_str(), path.c_str()) != 0) {
    std::remove(tmp.c_str());
    throw WavIOError("报告原子替换失败: " + path, true);
  }
}

std::vector<std::string> build_summary_lines(const RepairResult& result,
                                             std::size_t frames) {
  const RepairStats& st = result.stats;
  std::ostringstream os;
  os << "events=" << st.event_count
     << " repaired_frames=" << st.repaired_frames
     << " channel_mask=";
  if (result.events.empty()) {
    os << "0";
  } else {
    for (std::size_t i = 0; i < result.events.size(); ++i) {
      if (i) os << ",";
      os << static_cast<unsigned>(result.events[i].channel_mask);
    }
  }
  os << " residual_energy=" << fmt_double(st.residual_energy)
     << " peak_before=";
  char buf[64];
  std::snprintf(buf, sizeof(buf), "%.6f", st.peak_before);
  os << buf;
  std::snprintf(buf, sizeof(buf), "%.6f", st.peak_after);
  os << " peak_after=" << buf << " frames=" << frames;
  return {os.str(), result.success ? "verify=PASS" : "verify=FAIL"};
}

StoredReport read_report(const std::string& path) {
  std::ifstream f(path, std::ios::binary);
  if (!f) throw WavIOError("无法打开报告: " + path, false);
  const std::string content((std::istreambuf_iterator<char>(f)),
                            std::istreambuf_iterator<char>());

  // 报告由本工具生成、结构受控；对每个需要的字段用独立探针按键查找。
  auto top_number = [&](const std::string& key) -> double {
    MiniJson probe(content);
    probe.expect('{');
    if (probe.find_key(key)) return probe.parse_number();
    return 0.0;
  };

  StoredReport rep;
  rep.sample_rate =
      static_cast<std::uint32_t>(top_number("sample_rate"));
  rep.channels = static_cast<std::uint16_t>(top_number("channels"));
  rep.frames = static_cast<std::size_t>(top_number("frames"));
  rep.threshold = top_number("threshold");
  rep.context = static_cast<std::size_t>(top_number("context"));

  // stats 子对象：find_key 命中后游标停在值起点，顺序取所需字段。
  {
    MiniJson p(content);
    p.expect('{');
    if (p.find_key("stats")) {
      p.expect('{');
      if (p.find_key("events"))
        rep.stats.event_count =
            static_cast<std::size_t>(p.parse_number());
      if (p.find_key("repaired_frames"))
        rep.stats.repaired_frames =
            static_cast<std::size_t>(p.parse_number());
      if (p.find_key("repaired_samples"))
        rep.stats.repaired_samples =
            static_cast<std::size_t>(p.parse_number());
      if (p.find_key("peak_before"))
        rep.stats.peak_before = p.parse_number();
      if (p.find_key("peak_after"))
        rep.stats.peak_after = p.parse_number();
      if (p.find_key("residual_energy"))
        rep.stats.residual_energy = p.parse_number();
    }
  }

  // events 数组：逐对象解析，未知键跳过。
  {
    MiniJson p(content);
    p.expect('{');
    if (p.find_key("events")) {
      p.expect('[');
      p.skip_ws();
      while (p.consume('{')) {
        EventRepair r;
        p.skip_ws();
        while (true) {
          const std::string key = p.parse_string();
          p.expect(':');
          if (key == "event_id")
            r.event_id = static_cast<std::size_t>(p.parse_number());
          else if (key == "channel_mask")
            r.channel_mask =
                static_cast<std::uint8_t>(p.parse_number());
          else if (key == "repair_start")
            r.repair_start =
                static_cast<std::size_t>(p.parse_number());
          else if (key == "repair_end")
            r.repair_end =
                static_cast<std::size_t>(p.parse_number());
          else if (key == "residual_energy")
            r.residual_energy = p.parse_number();
          else
            p.skip_value();
          p.skip_ws();
          if (!p.consume(',')) break;
          p.skip_ws();
        }
        p.expect('}');
        rep.repairs.push_back(r);
        p.skip_ws();
        if (!p.consume(',')) break;
        p.skip_ws();
      }
    }
  }
  return rep;
}

}  // namespace popfix
