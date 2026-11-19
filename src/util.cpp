#include "util.hpp"
#include "sha256.hpp"
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <sstream>
#include <unistd.h>

namespace md {

void put_u16(std::vector<uint8_t>& out, uint16_t v) {
    out.push_back(uint8_t(v)); out.push_back(uint8_t(v >> 8));
}
void put_u32(std::vector<uint8_t>& out, uint32_t v) {
    for (int i = 0; i < 4; ++i) out.push_back(uint8_t(v >> (8 * i)));
}
void put_u64(std::vector<uint8_t>& out, uint64_t v) {
    for (int i = 0; i < 8; ++i) out.push_back(uint8_t(v >> (8 * i)));
}
uint16_t get_u16(const uint8_t* p) {
    return uint16_t(p[0]) | uint16_t(p[1] << 8);
}
uint32_t get_u32(const uint8_t* p) {
    return uint32_t(p[0]) | (uint32_t(p[1]) << 8)
         | (uint32_t(p[2]) << 16) | (uint32_t(p[3]) << 24);
}
uint64_t get_u64(const uint8_t* p) {
    uint64_t v = 0;
    for (int i = 0; i < 8; ++i) v |= uint64_t(p[i]) << (8 * i);
    return v;
}

uint64_t wrap_delta(uint64_t a, uint64_t b, uint64_t mod) {
    return (a - b) & (mod - 1);
}
int64_t wrap_signed_delta(uint64_t a, uint64_t b, uint64_t mod) {
    uint64_t d = wrap_delta(a, b, mod);
    uint64_t half = mod / 2;
    return int64_t(d < half ? d : d - mod);
}

std::string json_escape(const std::string& s) {
    std::string out;
    out.reserve(s.size() + 2);
    for (char c : s) {
        switch (c) {
            case '"':  out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (uint8_t(c) < 0x20) {
                    char buf[8];
                    std::snprintf(buf, sizeof(buf), "\\u%04x", c);
                    out += buf;
                } else out.push_back(c);
        }
    }
    return out;
}

bool atomic_write(const std::string& final_path,
                  const std::vector<uint8_t>& data, std::string& err) {
    namespace fs = std::filesystem;
    fs::path target(final_path);
    if (target.has_parent_path()) {
        std::error_code ec;
        fs::create_directories(target.parent_path(), ec);
    }
    fs::path tmp = target;
    tmp += ".tmp-";
    // 进程内唯一临时名（PID + 缓冲区地址熵）
    std::ostringstream tag;
    tag << getpid() << "-" << reinterpret_cast<uintptr_t>(data.data());
    tmp += tag.str();
    std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
    if (!f) { err = "cannot open temp file: " + tmp.string(); return false; }
    f.write(reinterpret_cast<const char*>(data.data()),
            static_cast<std::streamsize>(data.size()));
    f.flush();
    if (!f) { err = "write failed: " + tmp.string(); return false; }
    f.close();
    std::error_code ec;
    fs::rename(tmp, target, ec);
    if (ec) { err = "rename failed: " + ec.message(); return false; }
    return true;
}

bool atomic_append(const std::string& path,
                   const std::vector<uint8_t>& data, std::string& err) {
    std::ofstream f(path, std::ios::binary | std::ios::app);
    if (!f) { err = "cannot open for append: " + path; return false; }
    f.write(reinterpret_cast<const char*>(data.data()),
            static_cast<std::streamsize>(data.size()));
    if (!f) { err = "append failed: " + path; return false; }
    return true;
}

void write_result_json(std::ostream& os, const PipelineResult& r,
                       const ProcessConfig& cfg) {
    os << "{\n";
    os << "  \"param_version\": " << PARAM_VERSION << ",\n";
    os << "  \"input_sha256\": \"" << Sha256::hex(r.input_sha256) << "\",\n";
    os << "  \"roundtrip_rate\": " << std::fixed << std::setprecision(4)
       << r.roundtrip_rate << ",\n";
    os << "  \"keyframe_hit_rate\": " << std::fixed << std::setprecision(4)
       << r.keyframe_hit_rate << ",\n";
    os << "  \"segment_count\": " << r.segment_count << ",\n";
    os << "  \"duplicate_count\": " << r.duplicate_count << ",\n";
    os << "  \"peak_queue_items\": " << r.peak_queue_items << ",\n";
    os << "  \"recovered_stage\": \"" << json_escape(r.recovered_stage) << "\",\n";
    os << "  \"source_packets\": " << r.source_packets << ",\n";
    os << "  \"delivered_packets\": " << r.delivered_packets << ",\n";
    os << "  \"lost_packets\": " << r.lost_packets << ",\n";
    os << "  \"duplicate_packets\": " << r.duplicate_packets << ",\n";
    os << "  \"untrusted_spans\": " << r.untrusted_spans << ",\n";
    os << "  \"queue_capacity\": " << cfg.queue_capacity << ",\n";
    os << "  \"segments\": [\n";
    for (size_t i = 0; i < r.segments.size(); ++i) {
        const auto& s = r.segments[i];
        os << "    {\"id\": " << s.id
           << ", \"segment_sha256\": \"" << s.sha256 << "\""
           << ", \"sample_summary\": \"" << s.sample_summary << "\""
           << ", \"begin_us\": " << s.begin_us
           << ", \"end_us\": " << s.end_us
           << ", \"packets\": " << s.packets
           << ", \"keyframes\": " << s.keyframes
           << ", \"keyframe_indexed\": " << (s.keyframe_indexed ? "true" : "false")
           << ", \"disposition\": \"" << disposition_name(s.disposition) << "\""
           << ", \"duplicate_of_other\": " << (s.duplicate_of_other ? "true" : "false")
           << ", \"duplicate_of\": " << s.duplicate_of
           << ", \"silence_intervals\": " << s.silence.size()
           << "}" << (i + 1 == r.segments.size() ? "" : ",") << "\n";
    }
    os << "  ],\n";
    os << "  \"errors\": [\n";
    for (size_t i = 0; i < r.errors.size(); ++i) {
        const auto& e = r.errors[i];
        os << "    {\"type\": \"" << err_name(e.type) << "\""
           << ", \"stage\": \"" << json_escape(e.stage) << "\""
           << ", \"seq\": " << e.seq
           << ", \"message\": \"" << json_escape(e.message) << "\"}"
           << (i + 1 == r.errors.size() ? "" : ",") << "\n";
    }
    os << "  ]\n}\n";
}

} // namespace md
