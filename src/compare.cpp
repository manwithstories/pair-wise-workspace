#include "compare.h"

#include "init_segment.h"
#include "json.h"

#include <algorithm>
#include <cstdio>
#include <sstream>
#include <vector>

namespace fmp4 {

namespace {

struct TrackSnapshot {
    uint32_t track_id = 0;
    uint32_t timescale = 0;
    std::string codec;
    bool has_trex = false;
    uint32_t sample_description_index = 0;
    uint32_t default_duration = 0;
    uint32_t default_size = 0;
    uint32_t default_flags = 0;
    double max_ms = 0.0;
    double mean_ms = 0.0;
};

struct Snapshot {
    uint64_t fragments = 0;
    bool contiguous = false;
    std::vector<uint32_t> duplicate;
    std::vector<uint32_t> missing;
    std::vector<uint32_t> out_of_order;
    std::vector<TrackSnapshot> tracks;
};

std::string fixed(double value) {
    char buffer[64];
    std::snprintf(buffer, sizeof(buffer), "%.6f", value);
    return buffer;
}

std::string quoted(const std::string& value) {
    std::string out = "\"";
    for (char c : value) {
        if (c == '"' || c == '\\') out.push_back('\\');
        if (static_cast<unsigned char>(c) < 0x20) out.push_back(' ');
        else out.push_back(c);
    }
    out.push_back('"');
    return out;
}

std::vector<uint32_t> idArray(const JsonValue& value) {
    std::vector<uint32_t> result;
    for (const JsonValue& item : value.array()) {
        const int64_t id = item.integer();
        if (id < 0 || id > 0xFFFFFFFFLL) {
            throw Error(ExitCode::ParseFailed, "fragment sequence is out of uint32 range");
        }
        result.push_back(static_cast<uint32_t>(id));
    }
    return result;
}

Snapshot parseSnapshot(const JsonValue& root) {
    Snapshot result;
    if (root.contains("fragments")) result.fragments = static_cast<uint64_t>(root.at("fragments").integer());
    else result.fragments = static_cast<uint64_t>(root.at("fragment_count").integer());
    const JsonValue& sequence = root.at("moof_sequence");
    result.contiguous = sequence.at("contiguous").boolean();
    result.duplicate = idArray(sequence.at("duplicate"));
    result.missing = idArray(sequence.at("missing"));
    if (sequence.contains("out_of_order")) {
        result.out_of_order = idArray(sequence.at("out_of_order"));
    }
    for (const JsonValue& item : root.at("tracks").array()) {
        TrackSnapshot track;
        track.track_id = static_cast<uint32_t>(item.at("track_id").integer());
        track.timescale = static_cast<uint32_t>(item.at("timescale").integer());
        track.codec = item.at("codec").string();
        if (item.contains("has_trex")) track.has_trex = item.at("has_trex").boolean();
        if (item.contains("default_sample_description_index")) {
            track.sample_description_index =
                static_cast<uint32_t>(item.at("default_sample_description_index").integer());
        }
        track.default_duration = static_cast<uint32_t>(item.at("default_sample_duration").integer());
        if (item.contains("default_sample_size")) {
            track.default_size = static_cast<uint32_t>(item.at("default_sample_size").integer());
        }
        if (item.contains("default_sample_flags")) {
            track.default_flags = static_cast<uint32_t>(item.at("default_sample_flags").integer());
        }
        track.max_ms = item.at("deviation_max_ms").number();
        track.mean_ms = item.at("deviation_mean_ms").number();
        result.tracks.push_back(std::move(track));
    }
    return result;
}

void writeIds(std::ostringstream& out, const std::vector<uint32_t>& values) {
    out << '[';
    for (size_t i = 0; i < values.size(); ++i) {
        if (i) out << ',';
        out << values[i];
    }
    out << ']';
}

void writeMismatch(std::ostringstream& out, bool& first, const std::string& field,
                   const std::string& left, const std::string& right) {
    if (!first) out << ',';
    first = false;
    out << "{\"field\":" << quoted(field) << ",\"left\":" << quoted(left)
        << ",\"right\":" << quoted(right) << '}';
}

}  // namespace

CompareOutput compareValidationJson(const std::string& before_json,
                                    const std::string& after_json) {
    const Snapshot before = parseSnapshot(JsonValue::parse(before_json, "before.json"));
    const Snapshot after = parseSnapshot(JsonValue::parse(after_json, "after.json"));
    std::ostringstream mismatches;
    bool first_mismatch = true;
    const bool same_count = before.tracks.size() == after.tracks.size();
    if (!same_count) {
        writeMismatch(mismatches, first_mismatch, "track_count",
                      std::to_string(before.tracks.size()), std::to_string(after.tracks.size()));
    }
    const size_t shared = std::min(before.tracks.size(), after.tracks.size());
    for (size_t i = 0; i < shared; ++i) {
        const TrackSnapshot& a = before.tracks[i];
        const TrackSnapshot& b = after.tracks[i];
        const std::string prefix = "track[" + std::to_string(i) + "].";
        if (a.track_id != b.track_id) {
            writeMismatch(mismatches, first_mismatch, prefix + "track_id",
                          std::to_string(a.track_id), std::to_string(b.track_id));
        }
        if (a.timescale != b.timescale) {
            writeMismatch(mismatches, first_mismatch, prefix + "timescale",
                          std::to_string(a.timescale), std::to_string(b.timescale));
        }
        if (a.codec != b.codec) {
            writeMismatch(mismatches, first_mismatch, prefix + "codec", a.codec, b.codec);
        }
        if (a.has_trex != b.has_trex) {
            writeMismatch(mismatches, first_mismatch, prefix + "has_trex",
                          a.has_trex ? "true" : "false", b.has_trex ? "true" : "false");
        }
        if (a.sample_description_index != b.sample_description_index) {
            writeMismatch(mismatches, first_mismatch, prefix + "default_sample_description_index",
                          std::to_string(a.sample_description_index),
                          std::to_string(b.sample_description_index));
        }
        if (a.default_duration != b.default_duration) {
            writeMismatch(mismatches, first_mismatch, prefix + "default_sample_duration",
                          std::to_string(a.default_duration), std::to_string(b.default_duration));
        }
        if (a.default_size != b.default_size) {
            writeMismatch(mismatches, first_mismatch, prefix + "default_sample_size",
                          std::to_string(a.default_size), std::to_string(b.default_size));
        }
        if (a.default_flags != b.default_flags) {
            writeMismatch(mismatches, first_mismatch, prefix + "default_sample_flags",
                          std::to_string(a.default_flags), std::to_string(b.default_flags));
        }
    }
    const bool init_compatible = first_mismatch;
    const bool count_equal = before.fragments == after.fragments;
    const bool passed = count_equal && init_compatible && after.contiguous;

    std::ostringstream out;
    out << "{\"fragments_before\":" << before.fragments
        << ",\"fragments_after\":" << after.fragments
        << ",\"fragment_count_equal\":" << (count_equal ? "true" : "false")
        << ",\"contiguous_before\":" << (before.contiguous ? "true" : "false")
        << ",\"contiguous_after\":" << (after.contiguous ? "true" : "false")
        << ",\"duplicate_before\":";
    writeIds(out, before.duplicate);
    out << ",\"duplicate_after\":";
    writeIds(out, after.duplicate);
    out << ",\"missing_before\":";
    writeIds(out, before.missing);
    out << ",\"missing_after\":";
    writeIds(out, after.missing);
    out
        << ",\"moof_sequence\":{\"contiguous\":" << (before.contiguous ? "true" : "false")
        << ",\"duplicate\":";
    writeIds(out, before.duplicate);
    out << ",\"missing\":";
    writeIds(out, before.missing);
    out << ",\"out_of_order\":";
    writeIds(out, before.out_of_order);
    out << ",\"after\":{\"contiguous\":" << (after.contiguous ? "true" : "false")
        << ",\"duplicate\":";
    writeIds(out, after.duplicate);
    out << ",\"missing\":";
    writeIds(out, after.missing);
    out << ",\"out_of_order\":";
    writeIds(out, after.out_of_order);
    out << "}}";
    out << ",\"init_compatible\":" << (init_compatible ? "true" : "false")
        << ",\"init_mismatches\":[" << mismatches.str() << "]";
    out << ",\"tracks\":[";
    for (size_t i = 0; i < after.tracks.size(); ++i) {
        if (i) out << ',';
        const TrackSnapshot& track = after.tracks[i];
        out << "{\"track_id\":" << track.track_id << ",\"timescale\":" << track.timescale
            << ",\"codec\":" << quoted(track.codec) << ",\"deviation_max_ms\":"
            << fixed(track.max_ms) << ",\"deviation_mean_ms\":" << fixed(track.mean_ms);
        if (i < before.tracks.size()) {
            out << ",\"before_deviation_max_ms\":" << fixed(before.tracks[i].max_ms)
                << ",\"before_deviation_mean_ms\":" << fixed(before.tracks[i].mean_ms);
        }
        out << '}';
    }
    out << "],\"sequence_fixed\":" << (!before.contiguous && after.contiguous ? "true" : "false")
        << ",\"passed\":" << (passed ? "true" : "false")
        << ",\"status\":\"" << (passed ? "ok" : "failed") << "\"}";
    return {out.str(), passed};
}

}  // namespace fmp4

