#include "repacker.h"

#include "box_tree.h"
#include "byte_reader.h"
#include "error.h"
#include "util.h"

#include <cstring>
#include <filesystem>
#include <fstream>
#include <limits>
#include <map>
#include <sstream>

namespace mp4 {

namespace {

struct TrunPatch {
    size_t output_position = 0;
    uint64_t sample_offset_from_mdat = 0;
};

struct TfhdInfo {
    uint32_t track_id = 0;
    bool has_base_offset = false;
    uint64_t base_offset = 0;
    bool default_base_is_moof = false;
};

TfhdInfo read_tfhd(const std::vector<uint8_t>& data, const BoxNode& tfhd) {
    ByteReader reader(data.data() + tfhd.payload_offset, static_cast<size_t>(tfhd.payload_size),
                      tfhd.payload_offset);
    uint8_t version = 0;
    uint32_t flags = 0;
    reader.read_version_flags(version, flags);
    TfhdInfo result;
    result.track_id = reader.read_u32();
    if ((flags & 0x000001) != 0) {
        result.has_base_offset = true;
        result.base_offset = reader.read_u64();
    }
    result.default_base_is_moof = (flags & 0x020000) != 0;
    return result;
}

void append_box_header(std::vector<uint8_t>& out, const std::string& type, size_t payload_size) {
    const uint64_t total = payload_size + 8;
    if (total > std::numeric_limits<uint32_t>::max()) {
        fail(ExitCode::FormatError, "rebuilt box exceeds 32-bit size");
    }
    put_u32(out, static_cast<uint32_t>(total));
    out.insert(out.end(), type.begin(), type.end());
}

void append_range(std::vector<uint8_t>& out, const std::vector<uint8_t>& source,
                  uint64_t offset, uint64_t size) {
    if (offset > source.size() || size > source.size() - offset) {
        fail(ExitCode::FormatError, "box range is outside source file at byte offset " +
                                        std::to_string(offset));
    }
    out.insert(out.end(), source.begin() + static_cast<std::ptrdiff_t>(offset),
               source.begin() + static_cast<std::ptrdiff_t>(offset + size));
}

void append_tfdt(std::vector<uint8_t>& out, uint64_t decode_time) {
    const bool version1 = decode_time > std::numeric_limits<uint32_t>::max();
    append_box_header(out, "tfdt", version1 ? 12 : 8);
    out.push_back(version1 ? 1 : 0);
    out.insert(out.end(), {0, 0, 0});
    if (version1) put_u64(out, decode_time);
    else put_u32(out, static_cast<uint32_t>(decode_time));
}

void patch_u32(std::vector<uint8_t>& out, size_t position, uint32_t value) {
    if (position + 4 > out.size()) fail(ExitCode::FormatError, "internal box patch out of bounds");
    out[position] = static_cast<uint8_t>(value >> 24);
    out[position + 1] = static_cast<uint8_t>(value >> 16);
    out[position + 2] = static_cast<uint8_t>(value >> 8);
    out[position + 3] = static_cast<uint8_t>(value);
}

uint32_t read_u32_at(const std::vector<uint8_t>& data, uint64_t offset) {
    if (offset > data.size() || data.size() - offset < 4) {
        fail(ExitCode::FormatError, "truncated field at byte offset " + std::to_string(offset));
    }
    return (static_cast<uint32_t>(data[offset]) << 24) |
           (static_cast<uint32_t>(data[offset + 1]) << 16) |
           (static_cast<uint32_t>(data[offset + 2]) << 8) | data[offset + 3];
}

uint32_t read_u24_at(const std::vector<uint8_t>& data, uint64_t offset) {
    if (offset > data.size() || data.size() - offset < 3) {
        fail(ExitCode::FormatError, "truncated field at byte offset " + std::to_string(offset));
    }
    return (static_cast<uint32_t>(data[offset]) << 16) |
           (static_cast<uint32_t>(data[offset + 1]) << 8) | data[offset + 2];
}

const SegmentTrack& indexed_track(const SegmentInfo& segment, uint32_t track_id) {
    for (const auto& track : segment.tracks) {
        if (track.track_id == track_id) return track;
    }
    fail(ExitCode::FormatError, "index has no decode time for track " + std::to_string(track_id));
}

std::vector<uint8_t> rebuild_traf(const std::vector<uint8_t>& source, const BoxNode& traf,
                                  const BoxNode& moof, const BoxNode& mdat,
                                  const SegmentInfo& segment, std::vector<TrunPatch>& patches) {
    const BoxNode* tfhd = traf.find("tfhd");
    const BoxNode* tfdt = traf.find("tfdt");
    if (tfhd == nullptr || tfdt == nullptr) {
        fail(ExitCode::FormatError, "traf missing tfhd or tfdt during repack");
    }
    const TfhdInfo tfhd_info = read_tfhd(source, *tfhd);
    const SegmentTrack& track = indexed_track(segment, tfhd_info.track_id);
    const uint64_t base_absolute = tfhd_info.has_base_offset ? tfhd_info.base_offset : moof.offset;

    std::vector<uint8_t> out;
    append_box_header(out, "traf", 0);
    for (const BoxNode& child : traf.children) {
        if (child.type == "tfdt") {
            append_tfdt(out, track.base_decode_time);
            continue;
        }
        if (child.type == "trun") {
            const size_t output_position = out.size();
            append_range(out, source, child.offset, child.size);
            const uint32_t flags = read_u24_at(source, child.offset + 9);
            if ((flags & 0x000001) != 0) {
                const int32_t data_offset = static_cast<int32_t>(read_u32_at(source, child.offset + 16));
                const int64_t absolute = static_cast<int64_t>(base_absolute) + data_offset;
                if (absolute < static_cast<int64_t>(mdat.offset) ||
                    absolute > static_cast<int64_t>(mdat.offset + mdat.size)) {
                    fail(ExitCode::FormatError, "trun data offset points outside mdat at byte offset " +
                                                    std::to_string(child.offset));
                }
                patches.push_back(TrunPatch{output_position + 16,
                                            static_cast<uint64_t>(absolute - mdat.offset)});
            }
            continue;
        }
        append_range(out, source, child.offset, child.size);
    }
    const uint32_t size = static_cast<uint32_t>(out.size());
    patch_u32(out, 0, size);
    return out;
}

std::vector<uint8_t> rebuild_moof(const std::vector<uint8_t>& source, const BoxNode& moof,
                                  const BoxNode& mdat, const SegmentInfo& segment,
                                  uint32_t new_sequence) {
    std::vector<TrunPatch> patches;
    std::vector<uint8_t> out;
    append_box_header(out, "moof", 0);
    for (const BoxNode& child : moof.children) {
        if (child.type == "mfhd") {
            const size_t start = out.size();
            append_range(out, source, child.offset, child.size);
            patch_u32(out, start + 12, new_sequence);
        } else if (child.type == "traf") {
            const size_t first_patch = patches.size();
            const std::vector<uint8_t> traf = rebuild_traf(source, child, moof, mdat, segment, patches);
            const size_t traf_start = out.size();
            for (size_t i = first_patch; i < patches.size(); ++i) {
                patches[i].output_position += traf_start;
            }
            out.insert(out.end(), traf.begin(), traf.end());
        } else {
            append_range(out, source, child.offset, child.size);
        }
    }
    patch_u32(out, 0, static_cast<uint32_t>(out.size()));
    for (const auto& patch : patches) {
        const uint64_t new_offset = static_cast<uint64_t>(out.size()) + patch.sample_offset_from_mdat;
        if (new_offset > static_cast<uint64_t>(std::numeric_limits<int32_t>::max())) {
            fail(ExitCode::FormatError, "recalculated trun offset exceeds signed 32-bit range");
        }
        patch_u32(out, patch.output_position, static_cast<uint32_t>(new_offset));
    }
    return out;
}

void write_all(std::ofstream& output, const std::vector<uint8_t>& bytes, const std::string& path) {
    output.write(reinterpret_cast<const char*>(bytes.data()),
                 static_cast<std::streamsize>(bytes.size()));
    if (!output) fail(ExitCode::WriteError, "cannot write output '" + path + "'");
}

}  // namespace

RepackResult repack_index(const SegmentIndex& index, const std::string& init_path,
                          const std::string& output_path) {
    const std::vector<uint8_t> init = read_file(init_path);
    BoxTree init_tree(init);
    if (init_tree.find_root("ftyp") == nullptr || init_tree.find_root("moov") == nullptr) {
        fail(ExitCode::FormatError, "initialization file is missing ftyp or moov");
    }
    ensure_parent_directory(output_path);
    const std::string temp_path = output_path + ".tmp";
    std::ofstream output(temp_path, std::ios::binary | std::ios::trunc);
    if (!output) fail(ExitCode::WriteError, "cannot open output '" + output_path + "'");
    RepackResult result;
    try {
        write_all(output, init, output_path);
        for (size_t i = 0; i < index.segments.size(); ++i) {
            const SegmentInfo& segment = index.segments[i];
            const std::vector<uint8_t> source = read_file(path_join(index.source_dir, segment.file));
            BoxTree tree(source);
            const BoxNode* moof = tree.find_root("moof");
            const BoxNode* mdat = tree.find_root("mdat");
            if (moof == nullptr || mdat == nullptr) {
                fail(ExitCode::FormatError, "fragment lost moof or mdat during repack: " + segment.file);
            }
            const std::vector<uint8_t> new_moof =
                rebuild_moof(source, *moof, *mdat, segment, static_cast<uint32_t>(i + 1));
            write_all(output, new_moof, output_path);
            output.write(reinterpret_cast<const char*>(source.data() + mdat->offset),
                         static_cast<std::streamsize>(mdat->size));
            if (!output) fail(ExitCode::WriteError, "cannot write output '" + output_path + "'");
            ++result.segments_written;
        }
        output.close();
        if (!output) fail(ExitCode::WriteError, "cannot finalize output '" + output_path + "'");
        std::error_code ec;
        std::filesystem::rename(temp_path, output_path, ec);
        if (ec) {
            std::filesystem::remove(temp_path, ec);
            fail(ExitCode::WriteError, "cannot replace output '" + output_path + "'");
        }
    } catch (...) {
        output.close();
        std::error_code ec;
        std::filesystem::remove(temp_path, ec);
        throw;
    }
    result.source_has_segment_issue = !index.missing_sequences.empty() ||
                                      index.duplicate_count > 0 || index.out_of_order_count > 0;
    return result;
}

}  // namespace mp4
