#include "media/manifest.hpp"

#include <cmath>
#include <fstream>

#include "media/fs_util.hpp"

namespace mediaqc {

namespace {
double to_dbfs(double v) {
    if (v <= 0.0) return -144.0;
    return 20.0 * std::log10(v);
}
}  // namespace

LevelStats measure_levels(const std::string& wav_path, int channels) {
    struct Acc {
        int channels = 1;
        uint32_t peak = 0;
        uint64_t sum_sq = 0;
        uint64_t count = 0;
    } acc;
    acc.channels = std::max(1, channels);
    auto on_chunk = [](const unsigned char* data, size_t size, void* user) {
        auto* a = static_cast<Acc*>(user);
        const size_t n = size / 2;  // s16 samples
        for (size_t i = 0; i < n; ++i) {
            const int16_t s = static_cast<int16_t>(data[2 * i] | (data[2 * i + 1] << 8));
            const uint32_t mag = static_cast<uint32_t>(std::abs(static_cast<int>(s)));
            a->peak = std::max(a->peak, mag);
            a->sum_sq += static_cast<uint64_t>(mag) * mag;
            ++a->count;
        }
    };
    // Skip the 44-byte WAV header (all outputs are plain PCM WAV).
    std::ifstream f(wav_path, std::ios::binary);
    if (!f) throw std::runtime_error("cannot measure levels: " + wav_path);
    f.seekg(44);
    std::vector<unsigned char> buf(1 << 20);
    while (f) {
        f.read(reinterpret_cast<char*>(buf.data()), static_cast<std::streamsize>(buf.size()));
        const auto n = f.gcount();
        if (n > 0) on_chunk(buf.data(), static_cast<size_t>(n), &acc);
    }
    LevelStats out;
    out.peak_dbfs = to_dbfs(acc.peak / 32768.0);
    out.rms_dbfs = acc.count
        ? to_dbfs(std::sqrt(static_cast<double>(acc.sum_sq) / acc.count) / 32768.0)
        : -144.0;
    return out;
}

namespace {

json::Object damage_json(const DamageRecord& d) {
    return json::Object{
        {"code", std::string(to_string(d.code))},
        {"source_offset", d.source_offset},
        {"start_sample", d.start_sample},
        {"end_sample", d.end_sample},
        {"detail", d.detail},
    };
}

json::Object segment_json(const Segment& s) {
    return json::Object{{"start_sample", s.start_sample}, {"end_sample", s.end_sample}};
}

json::Object track_json(const TrackManifest& t) {
    json::Array damage;
    for (const auto& d : t.damage) damage.emplace_back(damage_json(d));
    json::Array ranges;
    for (const auto& s : t.policy_ranges) ranges.emplace_back(segment_json(s));
    return json::Object{
        {"track_id", t.track_id},
        {"source_path", t.source_path},
        {"output_path", t.output_path},
        {"start_ns", t.start_ns},
        {"end_ns", t.end_ns},
        {"packets", t.packets},
        {"damaged_ranges", t.damaged_ranges},
        {"truncated_bytes", t.truncated_bytes},
        {"target_bitrate", t.target_bitrate},
        {"actual_bitrate", t.actual_bitrate},
        {"bitrate_deviation_pct", t.bitrate_deviation_pct},
        {"peak_dbfs", t.peak_dbfs},
        {"rms_dbfs", t.rms_dbfs},
        {"alignment_error_ns", t.alignment_error_ns},
        {"pcm_reused", t.pcm_reused},
        {"damage_policy", t.damage_policy},
        {"index_rebuilt", t.index_rebuilt},
        {"damage", json::Value(std::move(damage))},
        {"policy_ranges", json::Value(std::move(ranges))},
    };
}

ErrorCode code_from_string(const std::string& s) {
    for (int i = 0; i <= static_cast<int>(ErrorCode::Unrecoverable); ++i) {
        auto c = static_cast<ErrorCode>(i);
        if (s == to_string(c)) return c;
    }
    return ErrorCode::Ok;
}

}  // namespace

std::string render_manifest(const BatchManifest& m) {
    json::Array tracks;
    for (const auto& t : m.tracks) tracks.emplace_back(track_json(t));
    json::Value root(json::Object{
        {"schema_version", 1},
        {"output_rate_hz", m.output_rate_hz},
        {"output_channels", m.output_channels},
        {"target_bitrate", m.target_bitrate},
        {"manifest_alignment_max_ns", m.max_alignment_error_ns},
        {"has_recoverable_damage", m.has_recoverable_damage},
        {"unrecoverable", m.unrecoverable},
        {"tracks", json::Value(std::move(tracks))},
    });
    return json::dump(root);
}

BatchManifest parse_manifest(const std::string& path) {
    const json::Value root = json::parse(fs::read_file(path));
    BatchManifest m;
    m.output_rate_hz = static_cast<int>(root.find("output_rate_hz")->as_int());
    m.output_channels = static_cast<int>(root.find("output_channels")->as_int());
    m.target_bitrate = root.find("target_bitrate")->as_int();
    m.max_alignment_error_ns = root.find("manifest_alignment_max_ns")->as_int();
    m.has_recoverable_damage = root.find("has_recoverable_damage")->as_bool();
    if (const auto* u = root.find("unrecoverable")) m.unrecoverable = u->as_bool();
    for (const auto& jt : root.find("tracks")->as_array()) {
        TrackManifest t;
        auto str = [&](const char* k) { return jt.find(k)->as_string(); };
        auto i64 = [&](const char* k) { return jt.find(k)->as_int(); };
        t.track_id = str("track_id");
        t.source_path = str("source_path");
        t.output_path = str("output_path");
        t.start_ns = i64("start_ns");
        t.end_ns = i64("end_ns");
        t.packets = i64("packets");
        t.damaged_ranges = i64("damaged_ranges");
        t.truncated_bytes = i64("truncated_bytes");
        t.target_bitrate = i64("target_bitrate");
        t.actual_bitrate = i64("actual_bitrate");
        t.bitrate_deviation_pct = jt.find("bitrate_deviation_pct")->as_double();
        t.peak_dbfs = jt.find("peak_dbfs")->as_double();
        t.rms_dbfs = jt.find("rms_dbfs")->as_double();
        t.alignment_error_ns = i64("alignment_error_ns");
        t.pcm_reused = jt.find("pcm_reused")->as_bool();
        t.damage_policy = str("damage_policy");
        t.index_rebuilt = jt.find("index_rebuilt")->as_bool();
        if (const auto* dj = jt.find("damage"))
            for (const auto& d : dj->as_array())
                t.damage.push_back({code_from_string(d.find("code")->as_string()),
                                    d.find("source_offset")->as_int(),
                                    d.find("start_sample")->as_int(),
                                    d.find("end_sample")->as_int(),
                                    d.find("detail")->as_string()});
        if (const auto* pj = jt.find("policy_ranges"))
            for (const auto& s : pj->as_array())
                t.policy_ranges.push_back({s.find("start_sample")->as_int(),
                                           s.find("end_sample")->as_int()});
        m.tracks.push_back(std::move(t));
    }
    return m;
}

}  // namespace mediaqc
