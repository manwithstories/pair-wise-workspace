#include "fragment_index.h"

#include <algorithm>
#include <limits>

namespace fmp4 {

namespace {

std::vector<uint8_t> payload(const BoxReader& reader, const BoxNode& box) {
    return reader.read(box.payloadOffset(), box.payloadSize());
}

uint32_t flags24(ByteReader& in) {
    return (static_cast<uint32_t>(in.u8()) << 16) |
           (static_cast<uint32_t>(in.u8()) << 8) | in.u8();
}

uint32_t parseSequence(const BoxReader& reader, const BoxNode& mfhd) {
    const std::vector<uint8_t> bytes = payload(reader, mfhd);
    ByteReader in(bytes.data(), bytes.size(), mfhd.payloadOffset());
    in.skip(4);
    return in.u32();
}

TfhdRecord parseTfhd(const BoxReader& reader, const BoxNode& box) {
    const std::vector<uint8_t> bytes = payload(reader, box);
    ByteReader in(bytes.data(), bytes.size(), box.payloadOffset());
    TfhdRecord tfhd;
    tfhd.version = in.u8();
    tfhd.flags = flags24(in);
    tfhd.track_id = in.u32();
    if (tfhd.flags & 0x000001) {
        tfhd.has_base_data_offset = true;
        tfhd.base_data_offset = in.u64();
    }
    if (tfhd.flags & 0x000002) {
        tfhd.has_sample_description_index = true;
        tfhd.sample_description_index = in.u32();
    }
    if (tfhd.flags & 0x000008) {
        tfhd.has_default_sample_duration = true;
        tfhd.default_sample_duration = in.u32();
    }
    if (tfhd.flags & 0x000010) {
        tfhd.has_default_sample_size = true;
        tfhd.default_sample_size = in.u32();
    }
    if (tfhd.flags & 0x000020) {
        tfhd.has_default_sample_flags = true;
        tfhd.default_sample_flags = in.u32();
    }
    return tfhd;
}

uint64_t parseTfdt(const BoxReader& reader, const BoxNode& box, uint8_t& version) {
    const std::vector<uint8_t> bytes = payload(reader, box);
    ByteReader in(bytes.data(), bytes.size(), box.payloadOffset());
    version = in.u8();
    in.skip(3);
    if (version == 0) {
        return in.u32();
    }
    if (version == 1) {
        return in.u64();
    }
    throw parseError("unsupported tfdt version", box.payloadOffset());
}

TrunRecord parseTrun(const BoxReader& reader, const BoxNode& box, const TfhdRecord& tfhd) {
    const std::vector<uint8_t> bytes = payload(reader, box);
    ByteReader in(bytes.data(), bytes.size(), box.payloadOffset());
    TrunRecord trun;
    trun.version = in.u8();
    trun.flags = flags24(in);
    trun.sample_count = in.u32();
    if (trun.flags & 0x000001) {
        trun.has_data_offset = true;
        trun.data_offset = trun.version == 0 ? static_cast<int64_t>(in.u32()) : in.i32();
    }
    uint32_t first_sample_flags = 0;
    bool has_first_sample_flags = false;
    if (trun.flags & 0x000004) {
        has_first_sample_flags = true;
        first_sample_flags = in.u32();
    }
    trun.samples.reserve(trun.sample_count);
    for (uint32_t i = 0; i < trun.sample_count; ++i) {
        SampleRecord sample;
        if (trun.flags & 0x000100) {
            sample.duration_present = true;
            sample.duration = in.u32();
        } else if (tfhd.has_default_sample_duration) {
            sample.duration = tfhd.default_sample_duration;
        }
        if (trun.flags & 0x000200) {
            sample.size_present = true;
            sample.size = in.u32();
        } else if (tfhd.has_default_sample_size) {
            sample.size = tfhd.default_sample_size;
        }
        if (trun.flags & 0x000400) {
            sample.flags_present = true;
            sample.flags = in.u32();
        } else if (i == 0 && has_first_sample_flags) {
            sample.flags = first_sample_flags;
        } else if (tfhd.has_default_sample_flags) {
            sample.flags = tfhd.default_sample_flags;
        }
        if (trun.flags & 0x000800) {
            sample.composition_time_present = true;
            sample.composition_time_offset =
                trun.version == 0 ? static_cast<int32_t>(in.u32()) : in.i32();
        }
        trun.samples.push_back(sample);
    }
    return trun;
}

TrafRecord parseTraf(const BoxReader& reader, const BoxNode& traf, uint64_t moof_offset,
                     const InitSegment* init) {
    TrafRecord result;
    const BoxNode* tfhd_box = findChild(traf, "tfhd");
    const BoxNode* tfdt_box = findChild(traf, "tfdt");
    if (tfhd_box == nullptr || tfdt_box == nullptr) {
        throw parseError("traf requires tfhd and tfdt", traf.offset);
    }
    result.tfhd = parseTfhd(reader, *tfhd_box);
    if (init != nullptr) {
        for (const TrackInit& track : init->tracks) {
            if (track.track_id != result.tfhd.track_id) continue;
            if (!result.tfhd.has_default_sample_duration) {
                result.tfhd.has_default_sample_duration = true;
                result.tfhd.default_sample_duration = track.default_sample_duration;
            }
            if (!result.tfhd.has_default_sample_size) {
                result.tfhd.has_default_sample_size = true;
                result.tfhd.default_sample_size = track.default_sample_size;
            }
            if (!result.tfhd.has_default_sample_flags) {
                result.tfhd.has_default_sample_flags = true;
                result.tfhd.default_sample_flags = track.default_sample_flags;
            }
            break;
        }
    }
    result.tfdt_base = parseTfdt(reader, *tfdt_box, result.tfdt_version);
    const uint64_t base = result.tfhd.has_base_data_offset ? result.tfhd.base_data_offset
                                                           : moof_offset;
    for (const BoxNode* trun_box : findChildren(traf, "trun")) {
        TrunRecord trun = parseTrun(reader, *trun_box, result.tfhd);
        if (trun.has_data_offset) {
            const int64_t start = static_cast<int64_t>(base) + trun.data_offset;
            if (start < 0) {
                throw parseError("negative trun data start", trun_box->payloadOffset());
            }
            trun.data_start = static_cast<uint64_t>(start);
        }
        result.truns.push_back(std::move(trun));
    }
    return result;
}

void resolveSampleOffsets(FragmentRecord& fragment, uint64_t file_size) {
    uint64_t cursor = fragment.has_mdat ? fragment.mdat_offset + fragment.mdat_header_size : 0;
    for (TrafRecord& traf : fragment.trafs) {
        for (TrunRecord& trun : traf.truns) {
            if (!trun.has_data_offset) {
                if (cursor == 0) {
                    throw parseError("trun has no data_offset and no mdat", fragment.moof_offset);
                }
                trun.data_start = cursor;
            }
            uint64_t sample_offset = trun.data_start;
            for (SampleRecord& sample : trun.samples) {
                sample.file_offset = sample_offset;
                if (sample.size > file_size - std::min(sample_offset, file_size)) {
                    throw parseError("sample payload extends beyond end of file", sample_offset);
                }
                sample_offset += sample.size;
            }
            if (sample_offset > file_size) {
                throw parseError("trun payload extends beyond end of file", trun.data_start);
            }
            if (cursor == 0 || sample_offset > cursor) {
                cursor = sample_offset;
            }
        }
    }
}

}  // namespace

uint64_t TrafRecord::sampleCount() const {
    uint64_t count = 0;
    for (const TrunRecord& trun : truns) {
        count += trun.sample_count;
    }
    return count;
}

uint64_t TrafRecord::sampleDuration() const {
    uint64_t duration = 0;
    for (const TrunRecord& trun : truns) {
        for (const SampleRecord& sample : trun.samples) {
            duration += sample.duration;
        }
    }
    return duration;
}

FragmentIndex FragmentIndex::parse(const BoxReader& reader, const BoxTree& tree,
                                   const InitSegment* init) {
    FragmentIndex index;
    const std::vector<BoxNode>& roots = tree.roots();
    for (size_t i = 0; i < roots.size(); ++i) {
        if (roots[i].type != "moof") {
            continue;
        }
        FragmentRecord fragment;
        fragment.moof_offset = roots[i].offset;
        fragment.moof_size = roots[i].size;
        const BoxNode* mfhd = findChild(roots[i], "mfhd");
        if (mfhd == nullptr) {
            throw parseError("moof requires mfhd", roots[i].offset);
        }
        fragment.sequence = parseSequence(reader, *mfhd);
        for (const BoxNode* traf : findChildren(roots[i], "traf")) {
            fragment.trafs.push_back(parseTraf(reader, *traf, fragment.moof_offset, init));
        }
        for (size_t j = i + 1; j < roots.size() && roots[j].type != "moof"; ++j) {
            if (roots[j].type == "mdat") {
                fragment.has_mdat = true;
                fragment.mdat_offset = roots[j].offset;
                fragment.mdat_size = roots[j].size;
                fragment.mdat_header_size = roots[j].header_size;
                break;
            }
        }
        resolveSampleOffsets(fragment, reader.fileSize());
        index.fragments_.push_back(std::move(fragment));
    }
    return index;
}

}  // namespace fmp4

