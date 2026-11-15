#include "nal_split.h"

#include <array>
#include <sstream>

namespace h264 {
namespace {

struct PendingNal {
    bool active = false;
    std::uint64_t offset = 0;
    std::uint64_t start_code_offset = 0;
    bool start_code_4byte = false;
};

void finish_nal(std::vector<NalUnit>& nals, PendingNal& pending,
                std::uint64_t end_offset, bool at_eof) {
    if (!pending.active) {
        return;
    }
    if (end_offset < pending.offset) {
        end_offset = pending.offset;
    }
    NalUnit nal;
    nal.index = nals.size();
    nal.offset = pending.offset;
    nal.start_code_offset = pending.start_code_offset;
    nal.size = end_offset - pending.offset;
    nal.start_code_4byte = pending.start_code_4byte;
    nal.at_eof = at_eof;
    nals.push_back(nal);
    pending.active = false;
}

}  // namespace

bool scan_nal_units(const std::string& path,
                    std::vector<NalUnit>& nals,
                    std::uint64_t& file_size,
                    std::string& error) {
    nals.clear();
    file_size = 0;
    std::ifstream file(path, std::ios::binary);
    if (!file) {
        error = "cannot open input file: " + path;
        return false;
    }

    std::array<char, 1U << 20U> buffer{};
    std::uint64_t base = 0;
    std::uint64_t zero_run = 0;
    PendingNal pending;
    while (file) {
        file.read(buffer.data(), static_cast<std::streamsize>(buffer.size()));
        const std::streamsize got = file.gcount();
        for (std::streamsize i = 0; i < got; ++i) {
            const std::uint64_t absolute = base + static_cast<std::uint64_t>(i);
            const auto byte = static_cast<unsigned char>(buffer[static_cast<std::size_t>(i)]);
            if (byte == 0) {
                ++zero_run;
                continue;
            }
            if (byte == 1 && zero_run >= 2) {
                const bool four_byte = zero_run >= 3;
                const std::uint64_t code_offset = absolute - (four_byte ? 3U : 2U);
                finish_nal(nals, pending, code_offset, false);
                pending.active = true;
                pending.offset = absolute + 1;
                pending.start_code_offset = code_offset;
                pending.start_code_4byte = four_byte;
                zero_run = 0;
                continue;
            }
            zero_run = 0;
        }
        base += static_cast<std::uint64_t>(got);
        if (!file && !file.eof()) {
            error = "read failed: " + path;
            return false;
        }
    }
    file_size = base;
    finish_nal(nals, pending, file_size, true);
    if (nals.empty()) {
        error = "no Annex B start code found: " + path;
        return false;
    }

    for (NalUnit& nal : nals) {
        if (nal.size < 1) {
            if (nal.at_eof) {
                nal.type = 0;
                nal.nal_ref_idc = 0;
                continue;
            }
            std::ostringstream out;
            out << "NAL " << nal.index << " at offset " << nal.offset
                << " has no header byte";
            error = out.str();
            return false;
        }
        file.clear();
        file.seekg(static_cast<std::streamoff>(nal.offset), std::ios::beg);
        unsigned char header = 0;
        file.read(reinterpret_cast<char*>(&header), 1);
        if (!file) {
            error = "cannot read NAL header at offset " + std::to_string(nal.offset);
            return false;
        }
        nal.nal_ref_idc = static_cast<std::uint8_t>((header >> 5U) & 3U);
        nal.type = static_cast<std::uint8_t>(header & 0x1FU);
    }
    return true;
}

bool read_nal_payload(std::ifstream& file,
                      const NalUnit& nal,
                      std::vector<std::uint8_t>& payload,
                      std::string& error) {
    if (nal.size > static_cast<std::uint64_t>(SIZE_MAX)) {
        error = "NAL payload is too large";
        return false;
    }
    payload.resize(static_cast<std::size_t>(nal.size));
    file.clear();
    file.seekg(static_cast<std::streamoff>(nal.offset), std::ios::beg);
    if (!payload.empty()) {
        file.read(reinterpret_cast<char*>(payload.data()),
                  static_cast<std::streamsize>(payload.size()));
    }
    if (!file) {
        error = "cannot read NAL payload at offset " + std::to_string(nal.offset);
        return false;
    }
    return true;
}

}  // namespace h264
