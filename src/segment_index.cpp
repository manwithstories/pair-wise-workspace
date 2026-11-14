#include "segment_index.h"

#include "box_tree.h"
#include "byte_reader.h"
#include "error.h"
#include "util.h"

#include <algorithm>
#include <cstring>
#include <limits>
#include <map>
#include <sstream>
#include <unordered_map>

namespace mp4 {

namespace {

constexpr uint32_t kTfhdBaseDataOffset = 0x000001;
constexpr uint32_t kTfhdSampleDuration = 0x000008;
constexpr uint32_t kTrunDataOffset = 0x000001;
constexpr uint32_t kTrunFirstFlags = 0x000004;
constexpr uint32_t kTrunDuration = 0x000100;
constexpr uint32_t kTrunSize = 0x000200;
constexpr uint32_t kTrunFlags = 0x000400;
constexpr uint32_t kTrunComposition = 0x000800;

uint32_t read_sequence(const std::vector<uint8_t>& data, const BoxNode& mfhd) {
    ByteReader reader(data.data() + mfhd.payload_offset, static_cast<size_t>(mfhd.payload_size),
                      mfhd.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    return reader.read_u32();
}

SegmentTrack parse_traf(const std::vector<uint8_t>& data, const BoxNode& traf,
                        const std::map<uint32_t, TrexDefaults>& trex) {
    const BoxNode* tfhd = traf.find("tfhd");
    const BoxNode* tfdt = traf.find("tfdt");
    if (tfhd == nullptr || tfdt == nullptr) {
        fail(ExitCode::FormatError, "traf missing tfhd or tfdt at byte offset " +
                                        std::to_string(traf.offset));
    }
    ByteReader tfhd_reader(data.data() + tfhd->payload_offset,
                           static_cast<size_t>(tfhd->payload_size), tfhd->payload_offset);
    uint8_t version = 0;
    uint32_t tfhd_flags = 0;
    tfhd_reader.read_version_flags(version, tfhd_flags);
    SegmentTrack track;
    track.track_id = tfhd_reader.read_u32();
    if ((tfhd_flags & kTfhdBaseDataOffset) != 0) tfhd_reader.skip(8);
    if ((tfhd_flags & 0x000002) != 0) tfhd_reader.skip(4);
    uint64_t default_duration = 0;
    if ((tfhd_flags & kTfhdSampleDuration) != 0) default_duration = tfhd_reader.read_u32();
    if ((tfhd_flags & 0x000010) != 0) tfhd_reader.skip(4);
    if ((tfhd_flags & 0x000020) != 0) tfhd_reader.skip(4);
    if (default_duration == 0) {
        const auto it = trex.find(track.track_id);
        if (it != trex.end()) default_duration = it->second.default_duration;
    }

    ByteReader tfdt_reader(data.data() + tfdt->payload_offset,
                           static_cast<size_t>(tfdt->payload_size), tfdt->payload_offset);
    uint8_t tfdt_version = 0;
    uint32_t tfdt_flags = 0;
    tfdt_reader.read_version_flags(tfdt_version, tfdt_flags);
    track.tfdt_version = tfdt_version;
    if (tfdt_version == 0) track.tfdt_raw = tfdt_reader.read_u32();
    else if (tfdt_version == 1) track.tfdt_raw = tfdt_reader.read_u64();
    else fail(ExitCode::FormatError, "unsupported tfdt version at byte offset " +
                                         std::to_string(tfdt->offset));

    bool saw_trun = false;
    for (const BoxNode* trun : traf.find_all("trun")) {
        saw_trun = true;
        ByteReader reader(data.data() + trun->payload_offset,
                          static_cast<size_t>(trun->payload_size), trun->payload_offset);
        uint8_t trun_version = 0;
        uint32_t flags = 0;
        reader.read_version_flags(trun_version, flags);
        track.trun_versions.push_back(trun_version);
        track.trun_signed = track.trun_signed || trun_version == 1;
        const uint32_t count = reader.read_u32();
        if ((flags & kTrunDataOffset) != 0) reader.skip(4);
        if ((flags & kTrunFirstFlags) != 0) reader.skip(4);
        if (count > std::numeric_limits<uint32_t>::max() - track.sample_count) {
            fail(ExitCode::FormatError, "trun sample count overflow at byte offset " +
                                            std::to_string(trun->offset));
        }
        for (uint32_t i = 0; i < count; ++i) {
            uint64_t duration = default_duration;
            if ((flags & kTrunDuration) != 0) duration = reader.read_u32();
            if ((flags & kTrunSize) != 0) reader.skip(4);
            if ((flags & kTrunFlags) != 0) reader.skip(4);
            if ((flags & kTrunComposition) != 0) {
                const int64_t offset = trun_version == 1 ? reader.read_i32()
                                                          : static_cast<int64_t>(reader.read_u32());
                if (track.sample_count == 0 && i == 0) {
                    track.composition_offset_min = offset;
                    track.composition_offset_max = offset;
                } else {
                    track.composition_offset_min = std::min(track.composition_offset_min, offset);
                    track.composition_offset_max = std::max(track.composition_offset_max, offset);
                }
            }
            if (duration == 0) track.indexable = false;
            if (duration > std::numeric_limits<uint64_t>::max() - track.duration) {
                fail(ExitCode::FormatError, "sample duration overflow at byte offset " +
                                                std::to_string(trun->offset));
            }
            track.duration += duration;
            ++track.sample_count;
        }
    }
    if (!saw_trun) track.indexable = false;
    if (track.sample_count == 0) track.indexable = false;
    return track;
}

SegmentInfo parse_segment_file(const std::string& path, const std::string& file, size_t order,
                               const std::map<uint32_t, TrexDefaults>& trex) {
    const std::vector<uint8_t> data = read_file(path);
    BoxTree tree(data);
    const BoxNode* moof = tree.find_root("moof");
    const BoxNode* mdat = tree.find_root("mdat");
    if (moof == nullptr) {
        fail(ExitCode::FormatError, "fragment has no moof at byte offset 0: " + path);
    }
    if (mdat == nullptr) {
        fail(ExitCode::FormatError, "fragment has no mdat at byte offset 0: " + path);
    }
    const BoxNode* mfhd = moof->find("mfhd");
    if (mfhd == nullptr) {
        fail(ExitCode::FormatError, "moof missing mfhd at byte offset " +
                                        std::to_string(moof->offset));
    }
    SegmentInfo segment;
    segment.file = file;
    segment.sequence = read_sequence(data, *mfhd);
    segment.natural_order = order;
    segment.moof_offset = moof->offset;
    segment.moof_size = moof->size;
    segment.mdat_offset = mdat->offset;
    segment.mdat_size = mdat->size;
    for (const BoxNode* traf : moof->find_all("traf")) {
        SegmentTrack track = parse_traf(data, *traf, trex);
        for (const auto& existing : segment.tracks) {
            if (existing.track_id == track.track_id) {
                fail(ExitCode::FormatError, "duplicate traf for track " +
                                                std::to_string(track.track_id) + " in " + file);
            }
        }
        segment.tracks.push_back(std::move(track));
    }
    if (segment.tracks.empty()) {
        fail(ExitCode::FormatError, "moof has no traf at byte offset " +
                                        std::to_string(moof->offset));
    }
    return segment;
}

void reconstruct_decode_times(std::vector<SegmentInfo>& segments) {
    std::map<uint32_t, uint64_t> last_end;
    constexpr uint64_t kWrap = 1ull << 32;
    for (auto& segment : segments) {
        for (auto& track : segment.tracks) {
            uint64_t candidate = track.tfdt_raw;
            const auto it = last_end.find(track.track_id);
            if (track.tfdt_version == 0 && it != last_end.end()) {
                while (candidate < it->second) {
                    if (candidate > std::numeric_limits<uint64_t>::max() - kWrap) {
                        fail(ExitCode::FormatError, "tfdt wrap overflow for track " +
                                                        std::to_string(track.track_id));
                    }
                    candidate += kWrap;
                }
            }
            track.base_decode_time = candidate;
            if (track.duration > std::numeric_limits<uint64_t>::max() - candidate) {
                fail(ExitCode::FormatError, "decode time overflow for track " +
                                                std::to_string(track.track_id));
            }
            last_end[track.track_id] = candidate + track.duration;
        }
    }
}

void build_track_summaries(SegmentIndex& index) {
    index.tracks.clear();
    for (const auto& init_track : index.init.tracks) {
        TrackSummary summary;
        summary.track_id = init_track.track_id;
        summary.timescale = init_track.timescale;
        summary.handler = init_track.handler;
        summary.codec = init_track.codec;
        for (const auto& segment : index.segments) {
            for (const auto& track : segment.tracks) {
                if (track.track_id != summary.track_id) continue;
                summary.tfdt_versions.insert(track.tfdt_version);
                for (uint8_t version : track.trun_versions) summary.trun_versions.insert(version);
                if (!track.indexable) ++summary.unresolved_segments;
            }
        }
        index.tracks.push_back(std::move(summary));
    }
}

Json versions_json(const std::set<uint8_t>& versions) {
    Json out = Json::array();
    for (uint8_t version : versions) out.push(Json::integer(version));
    return out;
}

Json segment_track_json(const SegmentTrack& track) {
    Json out = Json::object();
    out.set("track_id", Json::integer(track.track_id));
    out.set("timescale", Json::integer(track.timescale));
    out.set("tfdt_version", Json::integer(track.tfdt_version));
    out.set("tfdt_raw", Json::integer(static_cast<int64_t>(track.tfdt_raw)));
    out.set("base_decode_time", Json::integer(static_cast<int64_t>(track.base_decode_time)));
    out.set("duration", Json::integer(static_cast<int64_t>(track.duration)));
    out.set("sample_count", Json::integer(track.sample_count));
    out.set("indexable", Json::boolean(track.indexable));
    out.set("trun_signed", Json::boolean(track.trun_signed));
    Json truns = Json::array();
    for (uint8_t version : track.trun_versions) truns.push(Json::integer(version));
    out.set("trun_versions", std::move(truns));
    out.set("composition_offset_min", Json::integer(track.composition_offset_min));
    out.set("composition_offset_max", Json::integer(track.composition_offset_max));
    return out;
}

uint64_t required_u64(const Json& object, const char* key) {
    const Json* value = object.find(key);
    if (value == nullptr) fail(ExitCode::FormatError, std::string("index missing field: ") + key);
    return value->as_u64();
}

std::string required_string(const Json& object, const char* key) {
    const Json* value = object.find(key);
    if (value == nullptr) fail(ExitCode::FormatError, std::string("index missing field: ") + key);
    return value->as_string();
}

}  // namespace

SegmentIndex build_index(const std::string& directory) {
    SegmentIndex index;
    index.source_dir = directory;
    const std::string init_path = path_join(directory, "init.mp4");
    index.init = parse_init_file(init_path);
    index.init_file = init_path;
    const std::map<uint32_t, TrexDefaults> trex = index.init.trex;

    const std::vector<std::string> files = list_regular_files(directory);
    uint32_t order = 0;
    for (const auto& file : files) {
        if (file == "init.mp4" ||
            (file.size() >= 5 && file.substr(file.size() - 5) == ".json")) {
            continue;
        }
        SegmentInfo segment = parse_segment_file(path_join(directory, file), file, order++, trex);
        for (const auto& init_track : index.init.tracks) {
            for (auto& track : segment.tracks) {
                if (track.track_id == init_track.track_id) track.timescale = init_track.timescale;
            }
        }
        index.segments.push_back(std::move(segment));
    }
    index.segments_found = static_cast<uint32_t>(index.segments.size());
    if (index.segments.empty()) fail(ExitCode::FormatError, "no fragment files found in " + directory);

    std::map<uint32_t, SegmentInfo> unique_by_sequence;
    std::vector<uint32_t> unique_scan_order;
    uint32_t max_seen = 0;
    for (auto& segment : index.segments) {
        auto inserted = unique_by_sequence.emplace(segment.sequence, segment);
        if (!inserted.second) {
            segment.duplicate = true;
            index.duplicate_sequences.push_back(segment.sequence);
            ++index.duplicate_count;
        } else {
            if (segment.sequence < max_seen) segment.out_of_order = true;
            max_seen = std::max(max_seen, segment.sequence);
            unique_scan_order.push_back(segment.sequence);
            inserted.first->second = segment;
        }
    }
    std::sort(index.duplicate_sequences.begin(), index.duplicate_sequences.end());
    index.duplicate_sequences.erase(
        std::unique(index.duplicate_sequences.begin(), index.duplicate_sequences.end()),
        index.duplicate_sequences.end());

    std::vector<SegmentInfo> unique;
    for (auto& pair : unique_by_sequence) {
        pair.second.duplicate = false;
        unique.push_back(pair.second);
    }
    std::map<uint32_t, size_t> scan_rank;
    for (size_t i = 0; i < unique_scan_order.size(); ++i) {
        scan_rank[unique_scan_order[i]] = i;
    }
    for (size_t i = 0; i < unique.size(); ++i) {
        if (scan_rank[unique[i].sequence] != i) {
            unique[i].out_of_order = true;
            ++index.out_of_order_count;
        }
    }
    index.segments_after_repack = static_cast<uint32_t>(unique.size());
    const uint32_t max_sequence = unique.empty() ? 0 : unique.back().sequence;
    for (uint32_t sequence = 1; sequence <= max_sequence; ++sequence) {
        if (unique_by_sequence.count(sequence) == 0) {
            index.missing_sequences.push_back(sequence);
            if (sequence == std::numeric_limits<uint32_t>::max()) break;
        }
    }
    index.seq_contiguous = index.missing_sequences.empty();
    reconstruct_decode_times(unique);
    index.segments = std::move(unique);
    build_track_summaries(index);
    return index;
}

Json index_to_json(const SegmentIndex& index) {
    Json root = Json::object();
    root.set("schema_version", Json::integer(1));
    root.set("source_dir", Json::string(index.source_dir));
    root.set("init_file", Json::string(index.init_file));
    root.set("init", init_to_json(index.init));
    root.set("segments_found", Json::integer(index.segments_found));
    root.set("segments_after_repack", Json::integer(index.segments_after_repack));
    Json missing = Json::array();
    for (uint32_t sequence : index.missing_sequences) missing.push(Json::integer(sequence));
    root.set("missing_sequences", std::move(missing));
    Json duplicates = Json::array();
    for (uint32_t sequence : index.duplicate_sequences) duplicates.push(Json::integer(sequence));
    root.set("duplicate_sequences", std::move(duplicates));
    root.set("duplicate_count", Json::integer(index.duplicate_count));
    root.set("out_of_order_count", Json::integer(index.out_of_order_count));
    root.set("seq_contiguous", Json::boolean(index.seq_contiguous));
    Json tracks = Json::array();
    for (const auto& track : index.tracks) {
        Json item = Json::object();
        item.set("track_id", Json::integer(track.track_id));
        item.set("timescale", Json::integer(track.timescale));
        item.set("handler", Json::string(track.handler));
        item.set("codec", Json::string(track.codec));
        item.set("tfdt_versions", versions_json(track.tfdt_versions));
        item.set("trun_versions", versions_json(track.trun_versions));
        item.set("unresolved_segments", Json::integer(track.unresolved_segments));
        tracks.push(std::move(item));
    }
    root.set("tracks", std::move(tracks));
    Json segments = Json::array();
    for (const auto& segment : index.segments) {
        Json item = Json::object();
        item.set("file", Json::string(segment.file));
        item.set("sequence", Json::integer(segment.sequence));
        item.set("natural_order", Json::integer(static_cast<int64_t>(segment.natural_order)));
        item.set("out_of_order", Json::boolean(segment.out_of_order));
        item.set("moof_offset", Json::integer(static_cast<int64_t>(segment.moof_offset)));
        item.set("moof_size", Json::integer(static_cast<int64_t>(segment.moof_size)));
        item.set("mdat_offset", Json::integer(static_cast<int64_t>(segment.mdat_offset)));
        item.set("mdat_size", Json::integer(static_cast<int64_t>(segment.mdat_size)));
        Json segment_tracks = Json::array();
        for (const auto& track : segment.tracks) segment_tracks.push(segment_track_json(track));
        item.set("tracks", std::move(segment_tracks));
        segments.push(std::move(item));
    }
    root.set("segments", std::move(segments));
    return root;
}

Json index_to_json_file(const SegmentIndex& index) { return index_to_json(index); }

InitInfo init_from_json(const Json& root, const std::string& source_path) {
    const Json* init_json = root.find("init");
    if (init_json == nullptr) fail(ExitCode::FormatError, "index missing init snapshot");
    InitInfo init;
    init.source_path = source_path;
    const Json* ftyp = init_json->find("ftyp");
    if (ftyp == nullptr) fail(ExitCode::FormatError, "index init missing ftyp");
    init.ftyp.major_brand = required_string(*ftyp, "major_brand");
    init.ftyp.minor_version = static_cast<uint32_t>(required_u64(*ftyp, "minor_version"));
    const Json* brands = ftyp->find("compatible_brands");
    if (brands != nullptr) {
        for (const auto& brand : brands->items()) init.ftyp.compatible_brands.push_back(brand.as_string());
    }
    init.movie_timescale = static_cast<uint32_t>(required_u64(*init_json, "movie_timescale"));
    init.movie_duration = required_u64(*init_json, "movie_duration");
    const Json* tracks = init_json->find("tracks");
    if (tracks == nullptr) fail(ExitCode::FormatError, "index init missing tracks");
    for (const auto& item : tracks->items()) {
        TrackInfo track;
        track.track_id = static_cast<uint32_t>(required_u64(item, "track_id"));
        track.timescale = static_cast<uint32_t>(required_u64(item, "timescale"));
        track.duration = required_u64(item, "duration");
        track.handler = required_string(item, "handler");
        track.codec = required_string(item, "codec");
        track.config_type = required_string(item, "config_type");
        const std::string config_hex = required_string(item, "config_hex");
        track.stsd_fingerprint = required_string(item, "stsd_hex");
        for (size_t i = 0; i + 1 < config_hex.size(); i += 2) {
            track.config_bytes.push_back(static_cast<uint8_t>(std::stoul(config_hex.substr(i, 2), nullptr, 16)));
        }
        init.tracks.push_back(std::move(track));
    }
    const Json* trex = init_json->find("trex");
    if (trex != nullptr) {
        for (const auto& item : trex->items()) {
            TrexDefaults defaults;
            defaults.track_id = static_cast<uint32_t>(required_u64(item, "track_id"));
            defaults.default_duration = static_cast<uint32_t>(required_u64(item, "default_duration"));
            defaults.default_size = static_cast<uint32_t>(required_u64(item, "default_size"));
            defaults.default_flags = static_cast<uint32_t>(required_u64(item, "default_flags"));
            init.trex[defaults.track_id] = defaults;
        }
    }
    return init;
}

SegmentIndex index_from_json(const Json& root) {
    SegmentIndex index;
    index.source_dir = required_string(root, "source_dir");
    const Json* init_file = root.find("init_file");
    if (init_file != nullptr) index.init_file = init_file->as_string();
    index.init = init_from_json(root, index.init_file);
    index.segments_found = static_cast<uint32_t>(required_u64(root, "segments_found"));
    index.segments_after_repack = static_cast<uint32_t>(required_u64(root, "segments_after_repack"));
    index.duplicate_count = static_cast<uint32_t>(required_u64(root, "duplicate_count"));
    index.out_of_order_count = static_cast<uint32_t>(required_u64(root, "out_of_order_count"));
    const Json* contiguous = root.find("seq_contiguous");
    index.seq_contiguous = contiguous != nullptr && contiguous->as_bool();
    for (const char* key : {"missing_sequences", "duplicate_sequences"}) {
        const Json* list = root.find(key);
        if (list == nullptr) continue;
        for (const auto& item : list->items()) {
            const uint32_t value = static_cast<uint32_t>(item.as_u64());
            if (std::strcmp(key, "missing_sequences") == 0) index.missing_sequences.push_back(value);
            else index.duplicate_sequences.push_back(value);
        }
    }
    const Json* tracks = root.find("tracks");
    if (tracks == nullptr) fail(ExitCode::FormatError, "index missing tracks");
    for (const auto& item : tracks->items()) {
        TrackSummary summary;
        summary.track_id = static_cast<uint32_t>(required_u64(item, "track_id"));
        summary.timescale = static_cast<uint32_t>(required_u64(item, "timescale"));
        summary.handler = required_string(item, "handler");
        summary.codec = required_string(item, "codec");
        const Json* tfdt_versions = item.find("tfdt_versions");
        if (tfdt_versions != nullptr) {
            for (const auto& version : tfdt_versions->items()) {
                summary.tfdt_versions.insert(static_cast<uint8_t>(version.as_u64()));
            }
        }
        const Json* trun_versions = item.find("trun_versions");
        if (trun_versions != nullptr) {
            for (const auto& version : trun_versions->items()) {
                summary.trun_versions.insert(static_cast<uint8_t>(version.as_u64()));
            }
        }
        const Json* unresolved = item.find("unresolved_segments");
        if (unresolved != nullptr) summary.unresolved_segments = static_cast<uint32_t>(unresolved->as_u64());
        index.tracks.push_back(std::move(summary));
    }
    const Json* segments = root.find("segments");
    if (segments == nullptr) fail(ExitCode::FormatError, "index missing segments");
    for (const auto& item : segments->items()) {
        SegmentInfo segment;
        segment.file = required_string(item, "file");
        segment.sequence = static_cast<uint32_t>(required_u64(item, "sequence"));
        segment.natural_order = static_cast<size_t>(required_u64(item, "natural_order"));
        const Json* ooo = item.find("out_of_order");
        segment.out_of_order = ooo != nullptr && ooo->as_bool();
        segment.moof_offset = required_u64(item, "moof_offset");
        segment.moof_size = required_u64(item, "moof_size");
        segment.mdat_offset = required_u64(item, "mdat_offset");
        segment.mdat_size = required_u64(item, "mdat_size");
        const Json* segment_tracks = item.find("tracks");
        if (segment_tracks == nullptr) fail(ExitCode::FormatError, "segment missing tracks");
        for (const auto& track_item : segment_tracks->items()) {
            SegmentTrack track;
            track.track_id = static_cast<uint32_t>(required_u64(track_item, "track_id"));
            track.timescale = static_cast<uint32_t>(required_u64(track_item, "timescale"));
            track.tfdt_version = static_cast<uint8_t>(required_u64(track_item, "tfdt_version"));
            track.tfdt_raw = required_u64(track_item, "tfdt_raw");
            track.base_decode_time = required_u64(track_item, "base_decode_time");
            track.duration = required_u64(track_item, "duration");
            track.sample_count = static_cast<uint32_t>(required_u64(track_item, "sample_count"));
            const Json* indexable = track_item.find("indexable");
            track.indexable = indexable == nullptr || indexable->as_bool(true);
            const Json* signed_offset = track_item.find("trun_signed");
            track.trun_signed = signed_offset != nullptr && signed_offset->as_bool();
            const Json* trun_versions = track_item.find("trun_versions");
            if (trun_versions != nullptr) {
                for (const auto& version : trun_versions->items()) {
                    track.trun_versions.push_back(static_cast<uint8_t>(version.as_u64()));
                }
            }
            const Json* cmin = track_item.find("composition_offset_min");
            if (cmin != nullptr) track.composition_offset_min = cmin->as_i64();
            const Json* cmax = track_item.find("composition_offset_max");
            if (cmax != nullptr) track.composition_offset_max = cmax->as_i64();
            segment.tracks.push_back(std::move(track));
        }
        index.segments.push_back(std::move(segment));
    }
    return index;
}

}  // namespace mp4
