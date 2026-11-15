#include "repack.h"

#include "box_tree.h"
#include "error.h"
#include "fragment_index.h"
#include "init_segment.h"

#include <filesystem>
#include <fstream>
#include <limits>
#include <map>
#include <vector>

namespace fmp4 {

namespace {

using Bytes = std::vector<uint8_t>;

void putU32(Bytes& out, uint32_t value) {
    out.push_back(static_cast<uint8_t>(value >> 24));
    out.push_back(static_cast<uint8_t>(value >> 16));
    out.push_back(static_cast<uint8_t>(value >> 8));
    out.push_back(static_cast<uint8_t>(value));
}

void putU64(Bytes& out, uint64_t value) {
    putU32(out, static_cast<uint32_t>(value >> 32));
    putU32(out, static_cast<uint32_t>(value));
}

void append(Bytes& target, const Bytes& source) {
    target.insert(target.end(), source.begin(), source.end());
}

Bytes makeBox(const char type[5], const Bytes& payload) {
    if (payload.size() > std::numeric_limits<uint32_t>::max() - 8) {
        throw Error(ExitCode::ParseFailed, "generated metadata box is too large");
    }
    Bytes box;
    box.reserve(payload.size() + 8);
    putU32(box, static_cast<uint32_t>(payload.size() + 8));
    box.insert(box.end(), type, type + 4);
    append(box, payload);
    return box;
}

Bytes fullBoxHeader(uint8_t version, uint32_t flags) {
    Bytes out;
    out.push_back(version);
    out.push_back(static_cast<uint8_t>(flags >> 16));
    out.push_back(static_cast<uint8_t>(flags >> 8));
    out.push_back(static_cast<uint8_t>(flags));
    return out;
}

Bytes makeMfhd(uint32_t sequence) {
    Bytes payload = fullBoxHeader(0, 0);
    putU32(payload, sequence);
    return makeBox("mfhd", payload);
}

Bytes makeTfhd(const TfhdRecord& tfhd) {
    uint32_t flags = 0x020000;
    if (tfhd.has_sample_description_index) flags |= 0x000002;
    Bytes payload = fullBoxHeader(0, flags);
    putU32(payload, tfhd.track_id);
    if (tfhd.has_sample_description_index) putU32(payload, tfhd.sample_description_index);
    return makeBox("tfhd", payload);
}

Bytes makeTfdt(uint64_t base) {
    Bytes payload = fullBoxHeader(1, 0);
    putU64(payload, base);
    return makeBox("tfdt", payload);
}

Bytes makeTrun(const TrunRecord& trun, int64_t data_offset) {
    bool has_composition = false;
    for (const SampleRecord& sample : trun.samples) {
        has_composition = has_composition || sample.composition_time_present;
    }
    uint32_t flags = 0x000001 | 0x000100 | 0x000200 | 0x000400;
    if (has_composition) flags |= 0x000800;
    Bytes payload = fullBoxHeader(1, flags);
    if (trun.samples.size() > std::numeric_limits<uint32_t>::max()) {
        throw Error(ExitCode::ParseFailed, "trun sample count exceeds uint32");
    }
    putU32(payload, static_cast<uint32_t>(trun.samples.size()));
    putU32(payload, static_cast<uint32_t>(static_cast<int32_t>(data_offset)));
    for (const SampleRecord& sample : trun.samples) {
        putU32(payload, sample.duration);
        putU32(payload, sample.size);
        putU32(payload, sample.flags);
        if (has_composition) {
            putU32(payload, static_cast<uint32_t>(sample.composition_time_offset));
        }
    }
    return makeBox("trun", payload);
}

Bytes buildMoof(const FragmentRecord& fragment, uint32_t sequence,
                const std::vector<int64_t>& offsets) {
    Bytes payload = makeMfhd(sequence);
    size_t run_index = 0;
    for (const TrafRecord& traf : fragment.trafs) {
        Bytes traf_payload = makeTfhd(traf.tfhd);
        append(traf_payload, makeTfdt(traf.tfdt_base));
        for (const TrunRecord& trun : traf.truns) {
            if (run_index >= offsets.size()) {
                throw Error(ExitCode::ParseFailed, "internal trun offset mismatch");
            }
            append(traf_payload, makeTrun(trun, offsets[run_index++]));
        }
        append(payload, makeBox("traf", traf_payload));
    }
    return makeBox("moof", payload);
}

void writeBytes(std::ostream& out, const Bytes& bytes) {
    out.write(reinterpret_cast<const char*>(bytes.data()),
              static_cast<std::streamsize>(bytes.size()));
    if (!out) throw ioError("failed to write repacked output");
}

void writeMdatHeader(std::ostream& out, uint64_t payload_size) {
    Bytes header;
    const uint64_t total = payload_size + 8;
    if (total <= std::numeric_limits<uint32_t>::max()) {
        putU32(header, static_cast<uint32_t>(total));
        header.insert(header.end(), {'m', 'd', 'a', 't'});
    } else {
        putU32(header, 1);
        header.insert(header.end(), {'m', 'd', 'a', 't'});
        putU64(header, payload_size + 16);
    }
    writeBytes(out, header);
}

void copySamples(const BoxReader& reader, const FragmentRecord& fragment, std::ostream& out,
                 uint64_t& bytes_written) {
    for (const TrafRecord& traf : fragment.trafs) {
        for (const TrunRecord& trun : traf.truns) {
            for (const SampleRecord& sample : trun.samples) {
                reader.copyRange(sample.file_offset, sample.size, out);
                bytes_written += sample.size;
            }
        }
    }
}

void writeFragment(const BoxReader& reader, const FragmentRecord& fragment, uint32_t sequence,
                   std::ostream& out, uint64_t& bytes_written) {
    std::vector<uint64_t> relative_offsets;
    uint64_t payload_size = 0;
    for (const TrafRecord& traf : fragment.trafs) {
        for (const TrunRecord& trun : traf.truns) {
            relative_offsets.push_back(payload_size);
            for (const SampleRecord& sample : trun.samples) payload_size += sample.size;
        }
    }
    const uint64_t mdat_header = payload_size + 8 <= std::numeric_limits<uint32_t>::max() ? 8 : 16;
    std::vector<int64_t> placeholder(relative_offsets.size(), 0);
    const Bytes sizing_moof = buildMoof(fragment, sequence, placeholder);
    std::vector<int64_t> offsets;
    offsets.reserve(relative_offsets.size());
    for (uint64_t offset : relative_offsets) {
        const uint64_t value = sizing_moof.size() + mdat_header + offset;
        if (value > static_cast<uint64_t>(std::numeric_limits<int32_t>::max())) {
            throw Error(ExitCode::ParseFailed, "trun data_offset exceeds signed 32-bit range");
        }
        offsets.push_back(static_cast<int64_t>(value));
    }
    const Bytes moof = buildMoof(fragment, sequence, offsets);
    writeBytes(out, moof);
    writeMdatHeader(out, payload_size);
    copySamples(reader, fragment, out, bytes_written);
}

}  // namespace

RepackStats repackFile(const std::string& input_path, const std::string& output_path,
                       const RepackOptions& options) {
    if (options.tfdt_version != 1) {
        throw Error(ExitCode::UsageOrIo, "only --tfdt-version 1 is supported");
    }
    if (std::filesystem::absolute(input_path) == std::filesystem::absolute(output_path)) {
        throw Error(ExitCode::UsageOrIo, "input and output paths must differ");
    }
    BoxReader reader(input_path);
    const BoxTree tree = BoxTree::parse(reader);
    const InitSegment init = InitSegment::parse(reader, tree);
    const FragmentIndex index = FragmentIndex::parse(reader, tree, &init);
    if (index.fragments().empty()) {
        throw Error(ExitCode::ParseFailed, "no movie fragments found");
    }

    std::map<uint64_t, const FragmentRecord*> by_moof;
    std::map<uint64_t, bool> skip_mdat;
    for (const FragmentRecord& fragment : index.fragments()) {
        by_moof[fragment.moof_offset] = &fragment;
        if (fragment.has_mdat) skip_mdat[fragment.mdat_offset] = true;
    }

    const std::string temporary = output_path + ".tmp";
    std::filesystem::path parent = std::filesystem::path(output_path).parent_path();
    if (!parent.empty()) std::filesystem::create_directories(parent);
    std::ofstream output(temporary, std::ios::binary | std::ios::trunc);
    if (!output) throw ioError("cannot open repacked output: " + output_path);

    RepackStats stats;
    uint64_t bytes_written = 0;
    uint32_t next_sequence = 1;
    for (const BoxNode& root : tree.roots()) {
        if (root.type == "moof") {
            const auto it = by_moof.find(root.offset);
            if (it == by_moof.end()) {
                throw Error(ExitCode::ParseFailed, "moof is missing from fragment index");
            }
            const uint32_t sequence = options.renumber ? next_sequence++ : it->second->sequence;
            writeFragment(reader, *it->second, sequence, output, bytes_written);
            ++stats.fragment_count;
        } else if (root.type == "mdat" && skip_mdat.count(root.offset) != 0) {
            continue;
        } else {
            reader.copyRange(root.offset, root.size, output);
            bytes_written += root.size;
        }
    }
    output.flush();
    if (!output) throw ioError("failed to flush repacked output: " + output_path);
    output.close();
    std::filesystem::remove(output_path);
    std::error_code error;
    std::filesystem::rename(temporary, output_path, error);
    if (error) {
        std::filesystem::remove(temporary);
        throw ioError("cannot finalize repacked output: " + error.message());
    }
    stats.bytes_written = bytes_written;
    return stats;
}

}  // namespace fmp4

