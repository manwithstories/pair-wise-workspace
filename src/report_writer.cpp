#include "report_writer.h"

#include "box_tree.h"
#include "byte_reader.h"
#include "error.h"
#include "util.h"

#include <algorithm>
#include <cmath>
#include <iomanip>
#include <limits>
#include <map>
#include <sstream>

namespace mp4 {

namespace {

struct RepackedMoof {
    uint32_t sequence = 0;
    std::map<uint32_t, uint64_t> decode_times;
};

uint32_t read_track_id(const std::vector<uint8_t>& data, const BoxNode& tfhd) {
    ByteReader reader(data.data() + tfhd.payload_offset, static_cast<size_t>(tfhd.payload_size),
                      tfhd.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    return reader.read_u32();
}

uint64_t read_decode_time(const std::vector<uint8_t>& data, const BoxNode& tfdt) {
    ByteReader reader(data.data() + tfdt.payload_offset, static_cast<size_t>(tfdt.payload_size),
                      tfdt.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    if (version == 0) return reader.read_u32();
    if (version == 1) return reader.read_u64();
    fail(ExitCode::FormatError, "repacked file has unsupported tfdt version at byte offset " +
                                    std::to_string(tfdt.payload_offset));
}

std::vector<RepackedMoof> parse_repacked(const std::string& path) {
    const std::vector<uint8_t> data = read_file(path);
    BoxTree tree(data);
    if (tree.find_root("ftyp") == nullptr || tree.find_root("moov") == nullptr) {
        fail(ExitCode::FormatError, "repacked file has no initialization segment");
    }
    std::vector<RepackedMoof> moofs;
    for (const BoxNode& moof : tree.roots()) {
        if (moof.type != "moof") continue;
        RepackedMoof item;
        const BoxNode* mfhd = moof.find("mfhd");
        if (mfhd == nullptr) fail(ExitCode::FormatError, "repacked moof has no mfhd");
        ByteReader mfhd_reader(data.data() + mfhd->payload_offset,
                               static_cast<size_t>(mfhd->payload_size), mfhd->payload_offset);
        uint8_t version = 0;
        uint32_t flags = 0;
        mfhd_reader.read_version_flags(version, flags);
        item.sequence = mfhd_reader.read_u32();
        for (const BoxNode* traf : moof.find_all("traf")) {
            const BoxNode* tfhd = traf->find("tfhd");
            const BoxNode* tfdt = traf->find("tfdt");
            if (tfhd == nullptr || tfdt == nullptr) {
                fail(ExitCode::FormatError, "repacked traf has no tfhd/tfdt");
            }
            const uint32_t track_id = read_track_id(data, *tfhd);
            if (item.decode_times.count(track_id) != 0) {
                fail(ExitCode::FormatError, "repacked moof has duplicate track " +
                                                std::to_string(track_id));
            }
            item.decode_times[track_id] = read_decode_time(data, *tfdt);
        }
        moofs.push_back(std::move(item));
    }
    return moofs;
}

std::string track_name(const TrackSummary& track, const std::vector<TrackSummary>& tracks) {
    std::string base = track.handler == "vide" ? "video" : (track.handler == "soun" ? "audio" : "track");
    uint32_t same = 0;
    for (const auto& other : tracks) {
        const std::string other_base = other.handler == "vide" ? "video"
                                      : (other.handler == "soun" ? "audio" : "track");
        if (other_base == base) ++same;
    }
    if (same > 1 || base == "track") return base + "_" + std::to_string(track.track_id);
    return base;
}

const SegmentTrack& indexed_track(const SegmentInfo& segment, uint32_t track_id) {
    for (const auto& track : segment.tracks) {
        if (track.track_id == track_id) return track;
    }
    fail(ExitCode::FormatError, "index has no track " + std::to_string(track_id));
}

}  // namespace

ReportResult analyze_repacked(const SegmentIndex& index, const std::string& repacked_path) {
    ReportResult report;
    report.segments_found = index.segments_found;
    report.missing_sequences = index.missing_sequences;
    report.seq_contiguous = index.seq_contiguous;
    report.tracks = static_cast<uint32_t>(index.tracks.size());
    const std::vector<RepackedMoof> moofs = parse_repacked(repacked_path);
    report.segments_after_repack = static_cast<uint32_t>(moofs.size());
    report.repacked_seq_contiguous = !moofs.empty();
    for (size_t i = 0; i < moofs.size(); ++i) {
        if (moofs[i].sequence != i + 1) report.repacked_seq_contiguous = false;
    }

    bool all_monotonic = true;
    for (const auto& summary : index.tracks) {
        TrackDeviation deviation;
        deviation.track_id = summary.track_id;
        deviation.name = track_name(summary, index.tracks);
        deviation.timescale = summary.timescale;
        double total = 0.0;
        uint64_t previous = 0;
        bool have_previous = false;
        for (size_t i = 0; i < moofs.size() && i < index.segments.size(); ++i) {
            const auto current = moofs[i].decode_times.find(summary.track_id);
            if (current == moofs[i].decode_times.end()) {
                all_monotonic = false;
                continue;
            }
            if (have_previous && current->second < previous) {
                deviation.monotonic = false;
                all_monotonic = false;
            }
            if (have_previous && i > 0 &&
                index.segments[i].sequence == index.segments[i - 1].sequence + 1) {
                const SegmentTrack& previous_track =
                    indexed_track(index.segments[i - 1], summary.track_id);
                const uint64_t expected_delta = previous_track.duration;
                const uint64_t actual_delta = current->second >= previous
                                                  ? current->second - previous
                                                  : previous - current->second;
                const double difference = actual_delta >= expected_delta
                                              ? static_cast<double>(actual_delta - expected_delta)
                                              : static_cast<double>(expected_delta - actual_delta);
                const double milliseconds = summary.timescale == 0
                                                ? 0.0
                                                : difference * 1000.0 / summary.timescale;
                deviation.max_deviation_ms = std::max(deviation.max_deviation_ms, milliseconds);
                total += milliseconds;
                ++deviation.samples_compared;
            }
            previous = current->second;
            have_previous = true;
        }
        if (deviation.samples_compared > 0) {
            deviation.average_deviation_ms = total / deviation.samples_compared;
        }
        report.deviations.push_back(deviation);
    }

    if (!index.missing_sequences.empty()) report.verdict = "missing_segment";
    else if (index.duplicate_count > 0 || index.out_of_order_count > 0) report.verdict = "out_of_order";
    else if (!report.repacked_seq_contiguous || !all_monotonic) report.verdict = "repack_error";
    else report.verdict = "ok";
    return report;
}

Json report_to_json(const ReportResult& report) {
    Json root = Json::object();
    root.set("verdict", Json::string(report.verdict));
    root.set("segments_found", Json::integer(report.segments_found));
    root.set("segments_after_repack", Json::integer(report.segments_after_repack));
    Json missing = Json::array();
    for (uint32_t sequence : report.missing_sequences) missing.push(Json::integer(sequence));
    root.set("missing_sequences", std::move(missing));
    root.set("seq_contiguous", Json::boolean(report.seq_contiguous));
    root.set("repacked_seq_contiguous", Json::boolean(report.repacked_seq_contiguous));
    root.set("tracks", Json::integer(report.tracks));
    Json deviations = Json::array();
    for (const auto& track : report.deviations) {
        Json item = Json::object();
        item.set("track_id", Json::integer(track.track_id));
        item.set("name", Json::string(track.name));
        item.set("timescale", Json::integer(track.timescale));
        item.set("max_deviation_ms", Json::number(track.max_deviation_ms));
        item.set("average_deviation_ms", Json::number(track.average_deviation_ms));
        item.set("monotonic", Json::boolean(track.monotonic));
        item.set("samples_compared", Json::integer(track.samples_compared));
        deviations.push(std::move(item));
    }
    root.set("deviations", std::move(deviations));
    return root;
}

std::string report_summary(const ReportResult& report) {
    std::ostringstream out;
    out << "segments_found=" << report.segments_found << '\n';
    out << "segments_after_repack=" << report.segments_after_repack << '\n';
    out << "missing_sequences=[";
    for (size_t i = 0; i < report.missing_sequences.size(); ++i) {
        if (i != 0) out << ',';
        out << report.missing_sequences[i];
    }
    out << "]\n";
    out << "seq_contiguous=" << (report.seq_contiguous ? "true" : "false") << '\n';
    out << "repacked_seq_contiguous=" << (report.repacked_seq_contiguous ? "true" : "false") << '\n';
    out << "tracks=" << report.tracks << '\n';
    out << std::fixed << std::setprecision(3);
    for (const auto& track : report.deviations) {
        out << track.name << ".max_deviation_ms=" << track.max_deviation_ms << '\n';
        out << track.name << ".avg_deviation_ms=" << track.average_deviation_ms << '\n';
    }
    out << "verdict=" << report.verdict << '\n';
    return out.str();
}

}  // namespace mp4
