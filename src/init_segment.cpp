#include "init_segment.h"

#include <algorithm>
#include <sstream>

namespace fmp4 {

namespace {

const BoxNode* requiredChild(const BoxNode& parent, const std::string& type,
                             uint64_t owner_offset) {
    const BoxNode* child = findChild(parent, type);
    if (child == nullptr) {
        throw parseError("missing required box '" + type + "'", owner_offset);
    }
    return child;
}

uint32_t parseTrackId(const BoxReader& reader, const BoxNode& tkhd) {
    const std::vector<uint8_t> payload = reader.read(tkhd.payloadOffset(), tkhd.payloadSize());
    ByteReader in(payload.data(), payload.size(), tkhd.payloadOffset());
    const uint8_t version = in.u8();
    in.skip(3);
    in.skip(version == 1 ? 16 : 8);
    return in.u32();
}

uint32_t parseTimescale(const BoxReader& reader, const BoxNode& mdhd) {
    const std::vector<uint8_t> payload = reader.read(mdhd.payloadOffset(), mdhd.payloadSize());
    ByteReader in(payload.data(), payload.size(), mdhd.payloadOffset());
    const uint8_t version = in.u8();
    in.skip(3);
    in.skip(version == 1 ? 16 : 8);
    return in.u32();
}

std::string parseHandler(const BoxReader& reader, const BoxNode& hdlr) {
    const std::vector<uint8_t> payload = reader.read(hdlr.payloadOffset(), hdlr.payloadSize());
    ByteReader in(payload.data(), payload.size(), hdlr.payloadOffset());
    in.skip(8);
    return in.fourCC();
}

std::string parseCodec(const BoxReader& reader, const BoxNode& stsd) {
    const std::vector<uint8_t> payload = reader.read(stsd.payloadOffset(), stsd.payloadSize());
    ByteReader in(payload.data(), payload.size(), stsd.payloadOffset());
    in.skip(4);
    const uint32_t entry_count = in.u32();
    if (entry_count == 0) {
        throw parseError("stsd contains no sample entries", stsd.payloadOffset());
    }
    const uint64_t entry_start = in.position();
    const uint32_t entry_size = in.u32();
    const std::string entry_type = in.fourCC();
    if (entry_size < 8 || entry_start + entry_size > payload.size()) {
        throw parseError("invalid stsd sample entry size", stsd.payloadOffset() + entry_start);
    }
    return entry_type;
}

void parseTrex(const BoxReader& reader, const BoxNode& trex, TrackInit& track) {
    const std::vector<uint8_t> payload = reader.read(trex.payloadOffset(), trex.payloadSize());
    ByteReader in(payload.data(), payload.size(), trex.payloadOffset());
    in.skip(4);
    const uint32_t track_id = in.u32();
    if (track_id != track.track_id) {
        return;
    }
    track.has_trex = true;
    track.default_sample_description_index = in.u32();
    track.default_sample_duration = in.u32();
    track.default_sample_size = in.u32();
    track.default_sample_flags = in.u32();
}

std::string numberString(uint64_t value) {
    return std::to_string(value);
}

}  // namespace

InitSegment InitSegment::parse(const BoxReader& reader, const BoxTree& tree) {
    const BoxNode* moov = nullptr;
    for (const BoxNode& root : tree.roots()) {
        if (root.type == "moov") {
            moov = &root;
            break;
        }
    }
    if (moov == nullptr) {
        throw parseError("missing moov initialization segment", 0);
    }

    InitSegment init;
    for (const BoxNode* trak : findChildren(*moov, "trak")) {
        const BoxNode& tkhd = *requiredChild(*trak, "tkhd", trak->offset);
        const BoxNode& mdia = *requiredChild(*trak, "mdia", trak->offset);
        const BoxNode& mdhd = *requiredChild(mdia, "mdhd", mdia.offset);

        TrackInit track;
        track.track_id = parseTrackId(reader, tkhd);
        track.timescale = parseTimescale(reader, mdhd);
        if (const BoxNode* hdlr = findChild(mdia, "hdlr")) {
            track.handler = parseHandler(reader, *hdlr);
        }
        const BoxNode& minf = *requiredChild(mdia, "minf", mdia.offset);
        const BoxNode& stbl = *requiredChild(minf, "stbl", minf.offset);
        const BoxNode& stsd = *requiredChild(stbl, "stsd", stbl.offset);
        track.codec = parseCodec(reader, stsd);
        init.tracks.push_back(track);
    }

    if (const BoxNode* mvex = findChild(*moov, "mvex")) {
        for (const BoxNode* trex : findChildren(*mvex, "trex")) {
            for (TrackInit& track : init.tracks) {
                parseTrex(reader, *trex, track);
            }
        }
    }
    return init;
}

std::vector<InitMismatch> compareInitSegments(const InitSegment& left,
                                              const InitSegment& right) {
    std::vector<InitMismatch> diffs;
    if (left.tracks.size() != right.tracks.size()) {
        diffs.push_back({"track_count", numberString(left.tracks.size()),
                         numberString(right.tracks.size())});
    }

    const size_t shared = std::min(left.tracks.size(), right.tracks.size());
    bool id_order_mismatch = false;
    for (size_t i = 0; i < shared; ++i) {
        if (left.tracks[i].track_id != right.tracks[i].track_id) {
            id_order_mismatch = true;
        }
    }
    if (id_order_mismatch) {
        std::ostringstream a;
        std::ostringstream b;
        for (size_t i = 0; i < left.tracks.size(); ++i) {
            if (i) a << ',';
            a << left.tracks[i].track_id;
        }
        for (size_t i = 0; i < right.tracks.size(); ++i) {
            if (i) b << ',';
            b << right.tracks[i].track_id;
        }
        diffs.push_back({"track_id_order", a.str(), b.str()});
    }

    for (size_t i = 0; i < shared; ++i) {
        const TrackInit& a = left.tracks[i];
        const TrackInit& b = right.tracks[i];
        const std::string prefix = "track[" + std::to_string(i) + "].";
        if (a.track_id != b.track_id) {
            diffs.push_back({prefix + "track_id", numberString(a.track_id),
                             numberString(b.track_id)});
        }
        if (a.timescale != b.timescale) {
            diffs.push_back({prefix + "timescale", numberString(a.timescale),
                             numberString(b.timescale)});
        }
        if (a.codec != b.codec) {
            diffs.push_back({prefix + "codec", a.codec, b.codec});
        }
        if (a.has_trex != b.has_trex) {
            diffs.push_back({prefix + "has_trex", a.has_trex ? "true" : "false",
                             b.has_trex ? "true" : "false"});
        }
        if (a.default_sample_description_index != b.default_sample_description_index) {
            diffs.push_back({prefix + "default_sample_description_index",
                             numberString(a.default_sample_description_index),
                             numberString(b.default_sample_description_index)});
        }
        if (a.default_sample_duration != b.default_sample_duration) {
            diffs.push_back({prefix + "default_sample_duration",
                             numberString(a.default_sample_duration),
                             numberString(b.default_sample_duration)});
        }
        if (a.default_sample_size != b.default_sample_size) {
            diffs.push_back({prefix + "default_sample_size",
                             numberString(a.default_sample_size),
                             numberString(b.default_sample_size)});
        }
        if (a.default_sample_flags != b.default_sample_flags) {
            diffs.push_back({prefix + "default_sample_flags",
                             numberString(a.default_sample_flags),
                             numberString(b.default_sample_flags)});
        }
    }
    return diffs;
}

}  // namespace fmp4

