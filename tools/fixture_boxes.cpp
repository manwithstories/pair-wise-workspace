#include "fixture_boxes.h"

#include <limits>
#include <stdexcept>

namespace fixture {

namespace {

void putU16(Bytes& out, uint16_t value) {
    out.push_back(static_cast<uint8_t>(value >> 8));
    out.push_back(static_cast<uint8_t>(value));
}

void putU32(Bytes& out, uint32_t value) {
    out.push_back(static_cast<uint8_t>(value >> 24));
    out.push_back(static_cast<uint8_t>(value >> 16));
    out.push_back(static_cast<uint8_t>(value >> 8));
    out.push_back(static_cast<uint8_t>(value));
}

void putI32(Bytes& out, int32_t value) { putU32(out, static_cast<uint32_t>(value)); }

void putU64(Bytes& out, uint64_t value) {
    putU32(out, static_cast<uint32_t>(value >> 32));
    putU32(out, static_cast<uint32_t>(value));
}

void append(Bytes& target, const Bytes& source) {
    target.insert(target.end(), source.begin(), source.end());
}

Bytes makeBox(const std::string& type, const Bytes& payload) {
    if (type.size() != 4 || payload.size() > std::numeric_limits<uint32_t>::max() - 8) {
        throw std::runtime_error("invalid fixture box");
    }
    Bytes box;
    putU32(box, static_cast<uint32_t>(payload.size() + 8));
    box.insert(box.end(), type.begin(), type.end());
    append(box, payload);
    return box;
}

Bytes fullBox(uint8_t version, uint32_t flags) {
    Bytes out{version, static_cast<uint8_t>(flags >> 16), static_cast<uint8_t>(flags >> 8),
              static_cast<uint8_t>(flags)};
    return out;
}

void appendMatrix(Bytes& out) {
    putU32(out, 0x00010000);
    putU32(out, 0);
    putU32(out, 0);
    putU32(out, 0);
    putU32(out, 0x00010000);
    putU32(out, 0);
    putU32(out, 0);
    putU32(out, 0);
    putU32(out, 0x40000000);
}

Bytes makeMvhd(uint32_t timescale) {
    Bytes payload = fullBox(0, 0);
    putU32(payload, 0);
    putU32(payload, 0);
    putU32(payload, timescale);
    putU32(payload, 0);
    putU32(payload, 0x00010000);
    putU16(payload, 0x0100);
    putU16(payload, 0);
    putU32(payload, 0);
    putU32(payload, 0);
    appendMatrix(payload);
    for (int i = 0; i < 6; ++i) putU32(payload, 0);
    putU32(payload, 2);
    return makeBox("mvhd", payload);
}

Bytes makeTkhd(uint32_t track_id) {
    Bytes payload = fullBox(0, 7);
    putU32(payload, 0);
    putU32(payload, 0);
    putU32(payload, track_id);
    putU32(payload, 0);
    putU32(payload, 0);
    putU32(payload, 0);
    putU32(payload, 0);
    putU16(payload, 0);
    putU16(payload, 0);
    putU16(payload, 0);
    putU16(payload, 0);
    appendMatrix(payload);
    putU32(payload, 1920U << 16);
    putU32(payload, 1080U << 16);
    return makeBox("tkhd", payload);
}

Bytes makeMdhd(uint32_t timescale) {
    Bytes payload = fullBox(0, 0);
    putU32(payload, 0);
    putU32(payload, 0);
    putU32(payload, timescale);
    putU32(payload, 0);
    putU16(payload, 0x55C4);
    putU16(payload, 0);
    return makeBox("mdhd", payload);
}

Bytes makeHdlr() {
    Bytes payload = fullBox(0, 0);
    putU32(payload, 0);
    payload.insert(payload.end(), {'v', 'i', 'd', 'e'});
    for (int i = 0; i < 3; ++i) putU32(payload, 0);
    const char name[] = "VideoHandler";
    payload.insert(payload.end(), name, name + sizeof(name));
    return makeBox("hdlr", payload);
}

Bytes makeSampleEntry(const Config& config) {
    Bytes entry;
    putU32(entry, 0);
    entry.insert(entry.end(), config.codec.begin(), config.codec.end());
    for (int i = 0; i < 6; ++i) entry.push_back(0);
    putU16(entry, 1);
    putU16(entry, 0);
    putU16(entry, 0);
    for (int i = 0; i < 3; ++i) putU32(entry, 0);
    putU16(entry, 1920);
    putU16(entry, 1080);
    putU32(entry, 0x00480000);
    putU32(entry, 0x00480000);
    putU32(entry, 0);
    putU16(entry, 1);
    for (int i = 0; i < 32; ++i) entry.push_back(0);
    putU16(entry, 0x0018);
    putU16(entry, 0xFFFF);

    Bytes codec_payload;
    if (config.codec == "hvc1" || config.codec == "hev1") {
        for (int i = 0; i < 23; ++i) codec_payload.push_back(0);
        append(entry, makeBox("hvcC", codec_payload));
    } else {
        codec_payload = {1, 0x64, 0, 0x1F, 0xFF, 0xE1, 0, 4, 0x67, 0x64, 0, 0x1F,
                         1, 0, 4, 0x68, 0xEE, 0x3C, 0x80};
        append(entry, makeBox("avcC", codec_payload));
    }
    Bytes size_field;
    putU32(size_field, static_cast<uint32_t>(entry.size()));
    size_field.insert(size_field.end(), entry.begin() + 4, entry.end());
    return size_field;
}

Bytes makeStsd(const Config& config) {
    Bytes payload = fullBox(0, 0);
    putU32(payload, 1);
    append(payload, makeSampleEntry(config));
    return makeBox("stsd", payload);
}

Bytes makeEmptyTable(const char* type, bool size_table = false) {
    Bytes payload = fullBox(0, 0);
    if (size_table) putU32(payload, 0);
    putU32(payload, 0);
    return makeBox(type, payload);
}

Bytes makeTrex(const Config& config) {
    Bytes payload = fullBox(0, 0);
    putU32(payload, 1);
    putU32(payload, 1);
    putU32(payload, config.default_duration);
    putU32(payload, config.default_size);
    putU32(payload, 0);
    return makeBox("trex", payload);
}

Bytes makeTrak(const Config& config) {
    Bytes stbl;
    append(stbl, makeStsd(config));
    append(stbl, makeEmptyTable("stts"));
    append(stbl, makeEmptyTable("stsc"));
    append(stbl, makeEmptyTable("stsz", true));
    append(stbl, makeEmptyTable("stco"));

    Bytes minf;
    Bytes vmhd = fullBox(0, 1);
    putU16(vmhd, 0);
    putU16(vmhd, 0);
    putU16(vmhd, 0);
    putU16(vmhd, 0);
    append(minf, makeBox("vmhd", vmhd));
    Bytes url = fullBox(0, 1);
    Bytes dref = fullBox(0, 0);
    putU32(dref, 1);
    append(dref, makeBox("url ", url));
    Bytes dinf;
    append(dinf, makeBox("dref", dref));
    append(minf, makeBox("dinf", dinf));
    append(minf, makeBox("stbl", stbl));

    Bytes mdia;
    append(mdia, makeMdhd(config.timescale));
    append(mdia, makeHdlr());
    append(mdia, makeBox("minf", minf));

    Bytes trak;
    append(trak, makeTkhd(1));
    append(trak, makeBox("mdia", mdia));
    return makeBox("trak", trak);
}

Bytes makeTfhd(const Config& config, bool negative_offset, uint64_t base_data_offset) {
    uint32_t flags = negative_offset ? 0x000001 : 0x020000;
    Bytes payload = fullBox(0, flags);
    putU32(payload, 1);
    if (negative_offset) putU64(payload, base_data_offset);
    return makeBox("tfhd", payload);
}

Bytes makeTfdt(uint64_t tfdt, uint8_t version) {
    Bytes payload = fullBox(version, 0);
    if (version == 0) putU32(payload, static_cast<uint32_t>(tfdt));
    else putU64(payload, tfdt);
    return makeBox("tfdt", payload);
}

Bytes makeTrun(const Config& config, int64_t data_offset, uint8_t version) {
    Bytes payload = fullBox(version, 0x000701);
    putU32(payload, config.samples_per_fragment);
    if (version == 0) putU32(payload, static_cast<uint32_t>(data_offset));
    else putI32(payload, static_cast<int32_t>(data_offset));
    for (uint32_t i = 0; i < config.samples_per_fragment; ++i) {
        putU32(payload, config.default_duration);
        putU32(payload, config.default_size);
        putU32(payload, 0x02000000);
    }
    return makeBox("trun", payload);
}

Bytes makeMoofPayload(const Config& config, uint32_t sequence, uint64_t tfdt,
                      uint8_t tfdt_version, uint8_t trun_version, bool negative_offset,
                      uint64_t base_data_offset, int64_t data_offset) {
    Bytes mfhd = fullBox(0, 0);
    putU32(mfhd, sequence);
    Bytes traf;
    append(traf, makeTfhd(config, negative_offset, base_data_offset));
    append(traf, makeTfdt(tfdt, tfdt_version));
    append(traf, makeTrun(config, data_offset, trun_version));
    Bytes moof;
    append(moof, makeBox("mfhd", mfhd));
    append(moof, makeBox("traf", traf));
    return moof;
}

}  // namespace

uint64_t fragmentPayloadSize(const Config& config) {
    return static_cast<uint64_t>(config.samples_per_fragment) * config.default_size;
}

Bytes buildInit(const Config& config) {
    Bytes ftyp_payload;
    ftyp_payload.insert(ftyp_payload.end(), {'i', 's', 'o', '6'});
    putU32(ftyp_payload, 0);
    ftyp_payload.insert(ftyp_payload.end(), {'i', 's', 'o', '6', 'i', 's', 'o', 'm'});
    ftyp_payload.insert(ftyp_payload.end(), {'m', 'p', '4', '1'});
    Bytes moov;
    append(moov, makeMvhd(config.timescale));
    append(moov, makeTrak(config));
    Bytes mvex;
    append(mvex, makeTrex(config));
    append(moov, makeBox("mvex", mvex));
    Bytes file = makeBox("ftyp", ftyp_payload);
    append(file, makeBox("moov", moov));
    return file;
}

Bytes buildMdatHeader(const Config&, uint64_t payload_size, bool large_mdat) {
    Bytes header;
    const uint64_t total = payload_size + (large_mdat ? 16 : 8);
    if (large_mdat) {
        putU32(header, 1);
        header.insert(header.end(), {'m', 'd', 'a', 't'});
        putU64(header, total);
    } else {
        putU32(header, static_cast<uint32_t>(total));
        header.insert(header.end(), {'m', 'd', 'a', 't'});
    }
    return header;
}

Bytes buildMoof(const Config& config, uint32_t sequence, uint64_t tfdt,
                uint8_t tfdt_version, uint8_t trun_version, bool negative_offset,
                uint64_t moof_offset, uint64_t mdat_header_size) {
    const uint64_t payload_size = fragmentPayloadSize(config);
    Bytes probe = makeMoofPayload(config, sequence, tfdt, tfdt_version, trun_version,
                                  negative_offset, 0, 0);
    const uint64_t moof_size = probe.size() + 8;
    if (negative_offset) {
        if (trun_version != 1) throw std::runtime_error("negative offset requires trun v1");
        const uint64_t base = moof_offset + moof_size + mdat_header_size + payload_size;
        return makeBox("moof", makeMoofPayload(config, sequence, tfdt, tfdt_version,
                                               trun_version, true, base,
                                               -static_cast<int64_t>(payload_size)));
    }
    return makeBox("moof", makeMoofPayload(config, sequence, tfdt, tfdt_version, trun_version,
                                           false, 0,
                                           static_cast<int64_t>(moof_size + mdat_header_size)));
}

}  // namespace fixture

