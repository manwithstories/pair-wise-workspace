#include "report.h"

#include <cstdio>
#include <sstream>

namespace fmp4 {

namespace {

std::string quoted(const std::string& value) {
    std::string out = "\"";
    for (unsigned char c : value) {
        switch (c) {
            case '"': out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\b': out += "\\b"; break;
            case '\f': out += "\\f"; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (c < 0x20) {
                    char buffer[7];
                    std::snprintf(buffer, sizeof(buffer), "\\u%04x", c);
                    out += buffer;
                } else {
                    out.push_back(static_cast<char>(c));
                }
        }
    }
    out.push_back('"');
    return out;
}

std::string fixed(double value) {
    char buffer[64];
    std::snprintf(buffer, sizeof(buffer), "%.6f", value);
    return buffer;
}

std::string boolText(bool value) { return value ? "true" : "false"; }

void writeIdArray(std::ostringstream& out, const std::vector<uint32_t>& values) {
    out << '[';
    for (size_t i = 0; i < values.size(); ++i) {
        if (i) out << ',';
        out << values[i];
    }
    out << ']';
}

void writeMissingRanges(std::ostringstream& out, const std::vector<uint32_t>& missing) {
    out << '[';
    size_t i = 0;
    bool first = true;
    while (i < missing.size()) {
        const uint32_t start = missing[i];
        uint32_t end = start;
        while (i + 1 < missing.size() && missing[i + 1] == end + 1) {
            ++i;
            end = missing[i];
        }
        if (!first) out << ',';
        first = false;
        out << "{\"start\":" << start << ",\"end\":" << end << '}';
        ++i;
    }
    out << ']';
}

void writeSequenceJson(std::ostringstream& out, const SequenceReport& sequence) {
    out << "{\"contiguous\":" << boolText(sequence.contiguous)
        << ",\"in_order\":" << boolText(sequence.in_order) << ",\"duplicate\":";
    writeIdArray(out, sequence.duplicate);
    out << ",\"missing\":";
    writeIdArray(out, sequence.missing);
    out << ",\"out_of_order\":";
    writeIdArray(out, sequence.out_of_order);
    out << ",\"missing_ranges\":";
    writeMissingRanges(out, sequence.missing);
    out << ",\"missing_details\":[";
    for (size_t i = 0; i < sequence.missing_details.size(); ++i) {
        const MissingFragment& item = sequence.missing_details[i];
        if (i) out << ',';
        out << "{\"sequence\":" << item.sequence << ",\"previous_sequence\":";
        if (item.has_previous) out << item.previous_sequence;
        else out << "null";
        out << ",\"previous_moof_offset\":";
        if (item.has_previous) out << item.previous_moof_offset;
        else out << "null";
        out << ",\"next_sequence\":";
        if (item.has_next) out << item.next_sequence;
        else out << "null";
        out << ",\"next_moof_offset\":";
        if (item.has_next) out << item.next_moof_offset;
        else out << "null";
        out << '}';
    }
    out << "]}";
}

void writeTrackJson(std::ostringstream& out, const TrackReport& track) {
    out << "{\"track_id\":" << track.track_id << ",\"timescale\":" << track.timescale
        << ",\"codec\":" << quoted(track.codec)
        << ",\"has_trex\":" << boolText(track.has_trex)
        << ",\"default_sample_description_index\":"
        << track.default_sample_description_index
        << ",\"default_sample_duration\":" << track.default_sample_duration
        << ",\"default_sample_size\":" << track.default_sample_size
        << ",\"default_sample_flags\":" << track.default_sample_flags
        << ",\"fragment_occurrences\":" << track.fragment_occurrences
        << ",\"sample_count\":" << track.sample_count
        << ",\"deviation_max_ticks\":" << fixed(track.deviation_max_ticks)
        << ",\"deviation_mean_ticks\":" << fixed(track.deviation_mean_ticks)
        << ",\"deviation_max_ms\":" << fixed(track.deviation_max_ms)
        << ",\"deviation_mean_ms\":" << fixed(track.deviation_mean_ms) << '}';
}

void writeInitTracks(std::ostringstream& out, const InitSegment& init) {
    out << '[';
    for (size_t i = 0; i < init.tracks.size(); ++i) {
        const TrackInit& track = init.tracks[i];
        if (i) out << ',';
        out << "{\"track_id\":" << track.track_id << ",\"timescale\":" << track.timescale
            << ",\"codec\":" << quoted(track.codec) << ",\"handler\":" << quoted(track.handler)
            << ",\"has_trex\":" << boolText(track.has_trex)
            << ",\"default_sample_description_index\":" << track.default_sample_description_index
            << ",\"default_sample_duration\":" << track.default_sample_duration
            << ",\"default_sample_size\":" << track.default_sample_size
            << ",\"default_sample_flags\":" << track.default_sample_flags << '}';
    }
    out << ']';
}

}  // namespace

std::string validationJson(const ValidationResult& result) {
    std::ostringstream out;
    out << "{\"fragments\":" << result.fragment_count
        << ",\"fragment_count\":" << result.fragment_count << ",\"moof_sequence\":";
    writeSequenceJson(out, result.sequence);
    out << ",\"tracks\":[";
    for (size_t i = 0; i < result.tracks.size(); ++i) {
        if (i) out << ',';
        writeTrackJson(out, result.tracks[i]);
    }
    out << "],\"init\":{\"track_count\":" << result.init.tracks.size()
        << ",\"tracks\":";
    writeInitTracks(out, result.init);
    out << "},\"init_compatibility\":{\"checked\":" << boolText(result.init_checked)
        << ",\"compatible\":" << boolText(result.init_compatible) << ",\"mismatches\":[";
    for (size_t i = 0; i < result.init_diffs.size(); ++i) {
        const InitMismatch& diff = result.init_diffs[i];
        if (i) out << ',';
        out << "{\"field\":" << quoted(diff.field) << ",\"left\":" << quoted(diff.left)
            << ",\"right\":" << quoted(diff.right) << '}';
    }
    out << "]},\"timing_monotonic\":" << boolText(result.timing_monotonic)
        << ",\"init_compatible\":" << boolText(result.init_compatible)
        << ",\"passed\":" << boolText(result.passed)
        << ",\"status\":" << quoted(result.passed ? "ok" : "failed") << '}';
    return out.str();
}

std::string validationText(const ValidationResult& result) {
    std::ostringstream out;
    out << "fragments: " << result.fragment_count << '\n';
    out << "moof_sequence: " << (result.sequence.contiguous ? "contiguous" : "not contiguous")
        << " (in_order=" << boolText(result.sequence.in_order) << ")\n";
    out << "  duplicate: ";
    for (size_t i = 0; i < result.sequence.duplicate.size(); ++i) {
        if (i) out << ',';
        out << result.sequence.duplicate[i];
    }
    out << "\n  missing: ";
    for (size_t i = 0; i < result.sequence.missing.size(); ++i) {
        if (i) out << ',';
        out << result.sequence.missing[i];
    }
    out << "\n  out_of_order: ";
    for (size_t i = 0; i < result.sequence.out_of_order.size(); ++i) {
        if (i) out << ',';
        out << result.sequence.out_of_order[i];
    }
    out << '\n';
    for (const MissingFragment& missing : result.sequence.missing_details) {
        out << "  missing " << missing.sequence << ": previous=";
        if (missing.has_previous) out << missing.previous_sequence << '@' << missing.previous_moof_offset;
        else out << "none";
        out << " next=";
        if (missing.has_next) out << missing.next_sequence << '@' << missing.next_moof_offset;
        else out << "none";
        out << '\n';
    }
    for (const TrackReport& track : result.tracks) {
        out << "track " << track.track_id << ": timescale=" << track.timescale
            << " codec=" << track.codec << " default_duration=" << track.default_sample_duration
            << " deviation_max=" << fixed(track.deviation_max_ticks) << " ticks ("
            << fixed(track.deviation_max_ms) << " ms) mean="
            << fixed(track.deviation_mean_ticks) << " ticks ("
            << fixed(track.deviation_mean_ms) << " ms)\n";
    }
    out << "init_tracks: " << result.init.tracks.size();
    if (result.init_checked) {
        out << " compatible=" << boolText(result.init_compatible) << " mismatches="
            << result.init_diffs.size();
    }
    out << "\ntiming_monotonic: " << boolText(result.timing_monotonic) << '\n';
    out << "status: " << (result.passed ? "PASS" : "FAIL") << '\n';
    return out.str();
}

std::string probeJson(const InitSegment& init, const FragmentIndex& index) {
    std::ostringstream out;
    out << "{\"tracks\":";
    writeInitTracks(out, init);
    out << ",\"fragments\":[";
    for (size_t i = 0; i < index.fragments().size(); ++i) {
        const FragmentRecord& fragment = index.fragments()[i];
        if (i) out << ',';
        out << "{\"sequence\":" << fragment.sequence << ",\"moof_offset\":" << fragment.moof_offset
            << ",\"moof_size\":" << fragment.moof_size << ",\"mdat_offset\":"
            << (fragment.has_mdat ? std::to_string(fragment.mdat_offset) : "null")
            << ",\"traf_count\":" << fragment.trafs.size() << '}';
    }
    out << "]}";
    return out.str();
}

std::string probeText(const InitSegment& init, const FragmentIndex& index) {
    std::ostringstream out;
    out << "tracks: " << init.tracks.size() << '\n';
    for (const TrackInit& track : init.tracks) {
        out << "  track_id=" << track.track_id << " timescale=" << track.timescale
            << " codec=" << track.codec << " default_duration=" << track.default_sample_duration
            << " trex=" << boolText(track.has_trex) << '\n';
    }
    out << "fragments: " << index.fragments().size() << '\n';
    for (const FragmentRecord& fragment : index.fragments()) {
        out << "  seq=" << fragment.sequence << " moof=" << fragment.moof_offset << '+'
            << fragment.moof_size << " mdat=";
        if (fragment.has_mdat) out << fragment.mdat_offset << '+' << fragment.mdat_size;
        else out << "none";
        out << " trafs=" << fragment.trafs.size() << '\n';
    }
    return out.str();
}

}  // namespace fmp4

