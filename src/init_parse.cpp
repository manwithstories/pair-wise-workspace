#include "init_parse.h"

#include "box_tree.h"
#include "byte_reader.h"
#include "error.h"
#include "util.h"

#include <sstream>

namespace mp4 {

namespace {

uint32_t read_timescale(const std::vector<uint8_t>& data, const BoxNode& box) {
    ByteReader reader(data.data() + box.payload_offset, static_cast<size_t>(box.payload_size),
                      box.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    if (version == 0) {
        reader.skip(8);
        return reader.read_u32();
    }
    if (version == 1) {
        reader.skip(16);
        return reader.read_u32();
    }
    fail(ExitCode::FormatError, "unsupported mdhd version at byte offset " +
                                    std::to_string(box.payload_offset));
}

uint64_t read_mdhd_duration(const std::vector<uint8_t>& data, const BoxNode& box, uint8_t version) {
    ByteReader reader(data.data() + box.payload_offset, static_cast<size_t>(box.payload_size),
                      box.payload_offset);
    uint8_t actual = 0;
    uint32_t flags = 0;
    reader.read_version_flags(actual, flags);
    reader.skip(version == 0 ? 8u : 16u);
    reader.skip(4);
    return version == 0 ? reader.read_u32() : reader.read_u64();
}

uint32_t read_track_id(const std::vector<uint8_t>& data, const BoxNode& box) {
    ByteReader reader(data.data() + box.payload_offset, static_cast<size_t>(box.payload_size),
                      box.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    if (version == 0) {
        reader.skip(8);
        return reader.read_u32();
    }
    if (version == 1) {
        reader.skip(16);
        return reader.read_u32();
    }
    fail(ExitCode::FormatError, "unsupported tkhd version at byte offset " +
                                    std::to_string(box.payload_offset));
}

std::string read_handler(const std::vector<uint8_t>& data, const BoxNode& box) {
    ByteReader reader(data.data() + box.payload_offset, static_cast<size_t>(box.payload_size),
                      box.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    reader.skip(4);
    const std::string_view handler = reader.read_bytes(4);
    return std::string(handler);
}

struct EntryBox {
    std::string type;
    uint64_t offset = 0;
    uint64_t size = 0;
    uint64_t header_size = 8;
};

EntryBox read_entry_box(const std::vector<uint8_t>& data, uint64_t offset, uint64_t limit) {
    if (limit - offset < 8) {
        fail(ExitCode::FormatError, "truncated sample entry at byte offset " + std::to_string(offset));
    }
    ByteReader reader(data.data() + offset, static_cast<size_t>(limit - offset), offset);
    const uint32_t size32 = reader.read_u32();
    EntryBox result;
    result.type = fourcc(reader.cursor());
    reader.skip(4);
    result.offset = offset;
    if (size32 == 0) {
        fail(ExitCode::FormatError, "sample entry has size 0 at byte offset " + std::to_string(offset));
    }
    if (size32 == 1) {
        result.size = reader.read_u64();
        result.header_size = 16;
    } else {
        result.size = size32;
    }
    if (result.size < result.header_size || result.size > limit - offset) {
        fail(ExitCode::FormatError, "sample entry size is invalid at byte offset " +
                                        std::to_string(offset));
    }
    return result;
}

uint64_t sample_entry_children_offset(const std::string& type) {
    if (type == "avc1" || type == "avc3" || type == "hvc1" || type == "hev1") {
        return 86;
    }
    if (type == "mp4a" || type == "ac-3" || type == "ec-3" || type == "Opus") {
        return 36;
    }
    return 8;
}

void parse_stsd(const std::vector<uint8_t>& data, const BoxNode& stsd, TrackInfo& track) {
    ByteReader reader(data.data() + stsd.payload_offset, static_cast<size_t>(stsd.payload_size),
                      stsd.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    const uint32_t entry_count = reader.read_u32();
    if (entry_count == 0) {
        fail(ExitCode::FormatError, "stsd has no sample entries at byte offset " +
                                        std::to_string(stsd.offset));
    }
    const EntryBox entry = read_entry_box(data, stsd.payload_offset + 8,
                                          stsd.payload_offset + stsd.payload_size);
    track.codec = entry.type;
    track.stsd_fingerprint = hex_encode(data.data() + stsd.offset, stsd.size);
    const uint64_t children = entry.offset + sample_entry_children_offset(entry.type);
    const uint64_t entry_end = entry.offset + entry.size;
    if (children > entry_end) {
        fail(ExitCode::FormatError, "sample entry is too short at byte offset " +
                                        std::to_string(entry.offset));
    }
    uint64_t child_offset = children;
    while (child_offset < entry_end) {
        const EntryBox child = read_entry_box(data, child_offset, entry_end);
        if (child.type == "avcC" || child.type == "hvcC" || child.type == "esds" ||
            child.type == "dOps" || child.type == "dac3" || child.type == "dec3") {
            if (track.config_bytes.empty()) {
                track.config_type = child.type;
                track.config_bytes.assign(data.begin() + static_cast<std::ptrdiff_t>(child.offset + child.header_size),
                                          data.begin() + static_cast<std::ptrdiff_t>(child.offset + child.size));
            }
        }
        child_offset += child.size;
    }
}

}  // namespace

const TrackInfo* InitInfo::track(uint32_t track_id) const {
    for (const auto& item : tracks) {
        if (item.track_id == track_id) return &item;
    }
    return nullptr;
}

InitInfo parse_init(const std::vector<uint8_t>& data, const std::string& source_path) {
    BoxTree tree(data);
    InitInfo init;
    init.source_path = source_path;

    const BoxNode* ftyp = tree.find_root("ftyp");
    if (ftyp == nullptr || ftyp->payload_size < 8 || (ftyp->payload_size - 8) % 4 != 0) {
        fail(ExitCode::FormatError, "missing or invalid ftyp box");
    }
    ByteReader ftyp_reader(data.data() + ftyp->payload_offset,
                           static_cast<size_t>(ftyp->payload_size), ftyp->payload_offset);
    init.ftyp.major_brand = std::string(ftyp_reader.read_bytes(4));
    init.ftyp.minor_version = ftyp_reader.read_u32();
    while (ftyp_reader.remaining() >= 4) {
        init.ftyp.compatible_brands.push_back(std::string(ftyp_reader.read_bytes(4)));
    }

    const BoxNode* moov = tree.find_root("moov");
    if (moov == nullptr) fail(ExitCode::FormatError, "missing moov box");
    const BoxNode* mvhd = moov->find("mvhd");
    if (mvhd == nullptr) fail(ExitCode::FormatError, "missing mvhd box");
    ByteReader mvhd_reader(data.data() + mvhd->payload_offset,
                           static_cast<size_t>(mvhd->payload_size), mvhd->payload_offset);
    uint8_t mvhd_version = 0;
    uint32_t mvhd_flags = 0;
    mvhd_reader.read_version_flags(mvhd_version, mvhd_flags);
    if (mvhd_version == 0) {
        mvhd_reader.skip(8);
        init.movie_timescale = mvhd_reader.read_u32();
        init.movie_duration = mvhd_reader.read_u32();
    } else if (mvhd_version == 1) {
        mvhd_reader.skip(16);
        init.movie_timescale = mvhd_reader.read_u32();
        init.movie_duration = mvhd_reader.read_u64();
    } else {
        fail(ExitCode::FormatError, "unsupported mvhd version at byte offset " +
                                        std::to_string(mvhd->payload_offset));
    }

    for (const BoxNode* trak : moov->find_all("trak")) {
        const BoxNode* tkhd = trak->find("tkhd");
        const BoxNode* mdia = trak->find("mdia");
        if (tkhd == nullptr || mdia == nullptr) {
            fail(ExitCode::FormatError, "trak missing tkhd or mdia at byte offset " +
                                            std::to_string(trak->offset));
        }
        const BoxNode* mdhd = mdia->find("mdhd");
        const BoxNode* hdlr = mdia->find("hdlr");
        const BoxNode* minf = mdia->find("minf");
        const BoxNode* stbl = minf == nullptr ? nullptr : minf->find("stbl");
        const BoxNode* stsd = stbl == nullptr ? nullptr : stbl->find("stsd");
        if (mdhd == nullptr || hdlr == nullptr || stsd == nullptr) {
            fail(ExitCode::FormatError, "trak missing mdhd, hdlr or stsd at byte offset " +
                                            std::to_string(trak->offset));
        }
        TrackInfo track;
        track.track_id = read_track_id(data, *tkhd);
        track.timescale = read_timescale(data, *mdhd);
        ByteReader mdhd_reader(data.data() + mdhd->payload_offset,
                               static_cast<size_t>(mdhd->payload_size), mdhd->payload_offset);
        uint8_t mdhd_version = 0;
        uint32_t mdhd_flags = 0;
        mdhd_reader.read_version_flags(mdhd_version, mdhd_flags);
        track.duration = read_mdhd_duration(data, *mdhd, mdhd_version);
        track.handler = read_handler(data, *hdlr);
        parse_stsd(data, *stsd, track);
        if (track.track_id == 0 || track.timescale == 0) {
            fail(ExitCode::FormatError, "track has zero id or timescale at byte offset " +
                                            std::to_string(trak->offset));
        }
        for (const auto& existing : init.tracks) {
            if (existing.track_id == track.track_id) {
                fail(ExitCode::FormatError, "duplicate track_id " + std::to_string(track.track_id));
            }
        }
        init.tracks.push_back(std::move(track));
    }
    if (init.tracks.empty()) fail(ExitCode::FormatError, "moov contains no tracks");

    const BoxNode* mvex = moov->find("mvex");
    if (mvex != nullptr) {
        for (const BoxNode* trex : mvex->find_all("trex")) {
            ByteReader reader(data.data() + trex->payload_offset,
                              static_cast<size_t>(trex->payload_size), trex->payload_offset);
            uint8_t version = 0;
            uint32_t flags = 0;
            reader.read_version_flags(version, flags);
            TrexDefaults defaults;
            defaults.track_id = reader.read_u32();
            defaults.sample_description_index = reader.read_u32();
            defaults.default_duration = reader.read_u32();
            defaults.default_size = reader.read_u32();
            defaults.default_flags = reader.read_u32();
            init.trex[defaults.track_id] = defaults;
        }
    }
    return init;
}

InitInfo parse_init_file(const std::string& path) {
    return parse_init(read_file(path), path);
}

Json init_to_json(const InitInfo& init) {
    Json root = Json::object();
    Json ftyp = Json::object();
    ftyp.set("major_brand", Json::string(init.ftyp.major_brand));
    ftyp.set("minor_version", Json::integer(init.ftyp.minor_version));
    Json brands = Json::array();
    for (const auto& brand : init.ftyp.compatible_brands) brands.push(Json::string(brand));
    ftyp.set("compatible_brands", std::move(brands));
    root.set("ftyp", std::move(ftyp));
    root.set("movie_timescale", Json::integer(init.movie_timescale));
    root.set("movie_duration", Json::integer(static_cast<int64_t>(init.movie_duration)));
    Json tracks = Json::array();
    for (const auto& track : init.tracks) {
        Json item = Json::object();
        item.set("track_id", Json::integer(track.track_id));
        item.set("timescale", Json::integer(track.timescale));
        item.set("duration", Json::integer(static_cast<int64_t>(track.duration)));
        item.set("handler", Json::string(track.handler));
        item.set("codec", Json::string(track.codec));
        item.set("config_type", Json::string(track.config_type));
        item.set("config_hex", Json::string(hex_encode(track.config_bytes.data(), track.config_bytes.size())));
        item.set("stsd_hex", Json::string(track.stsd_fingerprint));
        tracks.push(std::move(item));
    }
    root.set("tracks", std::move(tracks));
    Json trex = Json::array();
    for (const auto& pair : init.trex) {
        Json item = Json::object();
        item.set("track_id", Json::integer(pair.second.track_id));
        item.set("default_duration", Json::integer(pair.second.default_duration));
        item.set("default_size", Json::integer(pair.second.default_size));
        item.set("default_flags", Json::integer(pair.second.default_flags));
        trex.push(std::move(item));
    }
    root.set("trex", std::move(trex));
    return root;
}

}  // namespace mp4
