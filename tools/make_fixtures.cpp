#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <stdexcept>
#include <string>
#include <vector>

namespace {

class BitWriter {
public:
    void u(std::uint64_t value, int bits) {
        for (int i = bits - 1; i >= 0; --i) {
            const std::uint8_t bit = static_cast<std::uint8_t>((value >> i) & 1U);
            if ((bit_pos_ & 7U) == 0) bytes_.push_back(0);
            bytes_.back() = static_cast<std::uint8_t>(bytes_.back() | (bit << (7 - (bit_pos_ & 7U))));
            ++bit_pos_;
        }
    }
    void ue(std::uint64_t value) {
        std::uint64_t code = value + 1;
        int bits = 0;
        for (std::uint64_t copy = code; copy > 0; copy >>= 1U) ++bits;
        u(0, bits - 1);
        u(code, bits);
    }
    void se(std::int64_t value) {
        const std::uint64_t code = value > 0 ? static_cast<std::uint64_t>(value) * 2U - 1U
                                             : static_cast<std::uint64_t>(-value) * 2U;
        ue(code);
    }
    void align_zero() { while ((bit_pos_ & 7U) != 0) u(0, 1); }
    void trailing_bits() { u(1, 1); align_zero(); }
    void append(const std::vector<std::uint8_t>& bytes) {
        align_zero();
        bytes_.insert(bytes_.end(), bytes.begin(), bytes.end());
        bit_pos_ += bytes.size() * 8U;
    }
    const std::vector<std::uint8_t>& data() const { return bytes_; }

private:
    std::vector<std::uint8_t> bytes_;
    std::size_t bit_pos_ = 0;
};

void write_start_code(std::ofstream& out, bool four_byte, unsigned extra_zeros = 0) {
    for (unsigned i = 0; i < extra_zeros; ++i) out.put(static_cast<char>(0));
    if (four_byte) out.put(static_cast<char>(0));
    out.put(static_cast<char>(0));
    out.put(static_cast<char>(0));
    out.put(static_cast<char>(1));
}

void write_nal(std::ofstream& out, std::uint8_t header, const std::vector<std::uint8_t>& rbsp,
               bool four_byte, unsigned extra_zeros = 0) {
    write_start_code(out, four_byte, extra_zeros);
    out.put(static_cast<char>(header));
    int zeros = 0;
    for (const std::uint8_t byte : rbsp) {
        if (zeros >= 2 && byte <= 3) {
            out.put(static_cast<char>(3));
            zeros = 0;
        }
        out.put(static_cast<char>(byte));
        if (byte == 0) ++zeros; else zeros = 0;
    }
}

void write_vui(BitWriter& b) {
    b.u(0, 1); b.u(0, 1); b.u(0, 1); b.u(0, 1);
    b.u(1, 1);
    b.u(1, 32);
    b.u(50, 32);
    b.u(1, 1);
    b.u(0, 1); b.u(0, 1); b.u(0, 1); b.u(0, 1);
}

void write_sps(std::ofstream& out, std::uint32_t width_mbs, std::uint32_t height_map_units,
               bool frame_mbs_only, bool mb_aff, std::uint32_t crop_bottom,
               std::uint32_t log2_frame_num = 4, std::uint32_t log2_poc_lsb = 4) {
    BitWriter b;
    b.u(100, 8); b.u(0, 8); b.u(41, 8); b.ue(0);
    b.ue(1); b.ue(0); b.ue(0); b.u(0, 1); b.u(0, 1);
    b.ue(log2_frame_num - 4); b.ue(0); b.ue(log2_poc_lsb - 4);
    b.ue(4); b.u(0, 1);
    b.ue(width_mbs - 1); b.ue(height_map_units - 1);
    b.u(frame_mbs_only ? 1 : 0, 1);
    if (!frame_mbs_only) b.u(mb_aff ? 1 : 0, 1);
    b.u(1, 1);
    b.u(crop_bottom == 0 ? 0 : 1, 1);
    if (crop_bottom != 0) { b.ue(0); b.ue(0); b.ue(0); b.ue(crop_bottom); }
    b.u(1, 1);
    write_vui(b);
    b.trailing_bits();
    write_nal(out, 0x67, b.data(), true);
}

void write_pps(std::ofstream& out) {
    BitWriter b;
    b.ue(0); b.ue(0); b.u(0, 1); b.u(0, 1); b.ue(0);
    b.ue(0); b.ue(0); b.u(0, 1); b.u(0, 2);
    b.se(0); b.se(0); b.se(0);
    b.u(1, 1); b.u(0, 1); b.u(0, 1);
    b.trailing_bits();
    write_nal(out, 0x68, b.data(), true);
}

struct SliceSpec {
    std::uint32_t first_mb = 0;
    int slice_type = 0;
    std::uint32_t frame_num = 0;
    std::uint32_t poc_lsb = 0;
    std::uint32_t frame_num_bits = 4;
    std::uint32_t poc_bits = 4;
    bool idr = false;
    bool frame_mbs_only = true;
    bool field_pic = false;
    bool bottom_field = false;
    bool four_byte = true;
    std::uint8_t nal_ref_idc = 3;
    std::uint32_t payload_seed = 0;
};

void write_slice(std::ofstream& out, const SliceSpec& spec) {
    BitWriter b;
    b.ue(spec.first_mb); b.ue(static_cast<std::uint64_t>(spec.slice_type)); b.ue(0);
    b.u(spec.frame_num, static_cast<int>(spec.frame_num_bits));
    if (!spec.frame_mbs_only) {
        b.u(spec.field_pic ? 1 : 0, 1);
        if (spec.field_pic) b.u(spec.bottom_field ? 1 : 0, 1);
    }
    if (spec.idr) b.ue(0);
    b.u(spec.poc_lsb, static_cast<int>(spec.poc_bits));
    if (!spec.idr && spec.slice_type == 1) b.u(0, 1);
    if (spec.slice_type != 2 && spec.slice_type != 4) {
        b.u(0, 1);
        b.u(0, 1);
        if (spec.slice_type == 1) b.u(0, 1);
    }
    if (spec.idr) { b.u(0, 1); b.u(0, 1); }
    else if (spec.nal_ref_idc != 0) b.u(0, 1);
    b.align_zero();
    for (int i = 0; i < 160; ++i) {
        b.u((spec.payload_seed * 37U + static_cast<std::uint32_t>(i) * 13U + 11U) & 0xFFU, 8);
    }
    const std::uint8_t header = static_cast<std::uint8_t>(spec.nal_ref_idc << 5U) |
                                static_cast<std::uint8_t>(spec.idr ? 5 : 1);
    write_nal(out, header, b.data(), spec.four_byte);
}

void write_aud(std::ofstream& out) {
    BitWriter b;
    b.u(7, 3);
    b.trailing_bits();
    write_nal(out, 0x09, b.data(), true);
}

void write_recovery_sei(std::ofstream& out) {
    BitWriter payload;
    payload.ue(0);
    payload.u(1, 1);
    payload.u(0, 1);
    payload.u(0, 2);
    payload.trailing_bits();
    BitWriter b;
    b.u(6, 8);
    b.u(payload.data().size(), 8);
    b.append(payload.data());
    b.trailing_bits();
    write_nal(out, 0x06, b.data(), false);
}

void write_mixed(const std::filesystem::path& path, std::uint32_t frames, std::uint32_t slices) {
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) throw std::runtime_error("cannot create " + path.string());
    const std::uint32_t gop_size = 30;
    const std::uint32_t gop_count = (frames + gop_size - 1) / gop_size;
    const std::vector<std::uint32_t> decode_order = {
        0, 1, 3, 2, 5, 4, 7, 6, 9, 8, 11, 10, 13, 12, 15, 14,
        16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29
    };
    for (std::uint32_t gop = 0; gop < gop_count; ++gop) {
        write_sps(out, 120, 68, true, false, 4);
        write_pps(out);
        for (std::uint32_t decode_index = 0; decode_index < gop_size; ++decode_index) {
            const std::uint32_t display = decode_order[decode_index];
            const bool is_i = display == 0;
            const bool is_b = display == 2 || display == 4 || display == 6 || display == 8 ||
                              display == 10 || display == 12 || display == 14;
            const int slice_type = is_i ? 2 : (is_b ? 1 : 0);
            const bool idr = is_i;
            const std::uint32_t frame_num = (gop * gop_size + decode_index) & 15U;
            const std::uint32_t poc_lsb = display & 15U;
            const std::uint32_t seed = gop * 131U + decode_index * 17U;
            for (std::uint32_t slice = 0; slice < slices; ++slice) {
                SliceSpec spec;
                spec.first_mb = slice;
                spec.slice_type = slice_type;
                spec.frame_num = frame_num;
                spec.poc_lsb = poc_lsb;
                spec.idr = idr;
                spec.four_byte = slice == 0;
                spec.nal_ref_idc = static_cast<std::uint8_t>(slice_type == 1 ? 0 : 3);
                spec.payload_seed = seed + slice;
                write_slice(out, spec);
            }
        }
    }
    if (!out) throw std::runtime_error("write failed: " + path.string());
}

void write_progressive(const std::filesystem::path& path, std::uint32_t frames,
                       std::uint32_t slices, bool aud, std::uint32_t recovery_period) {
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) throw std::runtime_error("cannot create " + path.string());
    write_sps(out, 120, 68, true, false, 4, 8, 8);
    write_pps(out);
    for (std::uint32_t frame = 0; frame < frames; ++frame) {
        const bool idr = frame == 0;
        const bool recovery = !idr && recovery_period != 0 && frame % recovery_period == 0;
        const int slice_type = (idr || recovery) ? 2 : 0;
        if (aud) write_aud(out);
        if (recovery) write_recovery_sei(out);
        for (std::uint32_t slice = 0; slice < slices; ++slice) {
            SliceSpec spec;
            spec.first_mb = slice;
            spec.slice_type = slice_type;
            spec.frame_num = frame & 255U;
            spec.poc_lsb = frame & 255U;
            spec.frame_num_bits = 8;
            spec.poc_bits = 8;
            spec.idr = idr;
            spec.four_byte = slice == 0;
            spec.payload_seed = frame * 29U + slice;
            write_slice(out, spec);
        }
    }
}

void write_paff(const std::filesystem::path& path, std::uint32_t frames) {
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) throw std::runtime_error("cannot create " + path.string());
    write_sps(out, 80, 23, false, false, 4, 8, 8);
    write_pps(out);
    for (std::uint32_t frame = 0; frame < frames; ++frame) {
        const bool idr = frame % 32U == 0;
        const bool recovery = !idr && (frame == 4 || frame == 8 || frame == 16 ||
                                       frame == 24 || frame == 40 || frame == 48 ||
                                       frame == 56 || frame == 72 || frame == 80 || frame == 88);
        const int slice_type = (idr || recovery) ? 2 : (frame % 7U == 3 ? 1 : 0);
        const std::uint8_t nal_ref_idc = slice_type == 1 ? 0 : 3;
        for (std::uint32_t field = 0; field < 2; ++field) {
            write_aud(out);
            if (field == 0 && recovery) write_recovery_sei(out);
            for (std::uint32_t slice = 0; slice < 2; ++slice) {
                SliceSpec spec;
                spec.first_mb = slice;
                spec.slice_type = slice_type;
                spec.frame_num = frame & 255U;
                spec.poc_lsb = frame & 255U;
                spec.frame_num_bits = 8;
                spec.poc_bits = 8;
                spec.idr = idr;
                spec.frame_mbs_only = false;
                spec.field_pic = true;
                spec.bottom_field = field == 1;
                spec.four_byte = field == 0 && slice == 0;
                spec.nal_ref_idc = nal_ref_idc;
                spec.payload_seed = frame * 43U + field * 7U + slice;
                write_slice(out, spec);
            }
        }
    }
}

void write_mbaff(const std::filesystem::path& path, std::uint32_t frames) {
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) throw std::runtime_error("cannot create " + path.string());
    write_sps(out, 80, 23, false, true, 4, 8, 8);
    write_pps(out);
    for (std::uint32_t frame = 0; frame < frames; ++frame) {
        const bool idr = frame % 32U == 0;
        const int slice_type = idr ? 2 : (frame % 6U == 3 ? 1 : 0);
        write_aud(out);
        for (std::uint32_t slice = 0; slice < 2; ++slice) {
            SliceSpec spec;
            spec.first_mb = slice;
            spec.slice_type = slice_type;
            spec.frame_num = frame & 255U;
            spec.poc_lsb = frame & 255U;
            spec.frame_num_bits = 8;
            spec.poc_bits = 8;
            spec.idr = idr;
            spec.frame_mbs_only = false;
            spec.four_byte = slice == 0;
            spec.nal_ref_idc = slice_type == 1 ? 0 : 3;
            spec.payload_seed = frame * 31U + slice;
            write_slice(out, spec);
        }
    }
}

void write_truncated(const std::filesystem::path& path) {
    write_progressive(path, 30, 2, false, 0);
    std::ofstream out(path, std::ios::binary | std::ios::app);
    if (!out) throw std::runtime_error("cannot append " + path.string());
    write_start_code(out, true);
    out.put(static_cast<char>(0x21));
    out.put(static_cast<char>(0));
}

std::uint32_t parse_u32(const std::string& text, const char* name) {
    try {
        const unsigned long value = std::stoul(text);
        if (value == 0 || value > 100000) throw std::out_of_range(name);
        return static_cast<std::uint32_t>(value);
    } catch (...) {
        throw std::runtime_error(std::string("invalid ") + name + ": " + text);
    }
}

}  // namespace

int main(int argc, char** argv) {
    try {
        std::string out_dir = "fixtures";
        std::uint32_t frames = 240;
        std::uint32_t slices = 3;
        std::uint32_t interlaced_pair = 2;
        for (int i = 1; i < argc; ++i) {
            const std::string arg = argv[i];
            if (arg == "--out" && i + 1 < argc) out_dir = argv[++i];
            else if (arg == "--frames" && i + 1 < argc) frames = parse_u32(argv[++i], "frames");
            else if (arg == "--slices" && i + 1 < argc) slices = parse_u32(argv[++i], "slices");
            else if (arg == "--interlaced-pair" && i + 1 < argc) interlaced_pair = parse_u32(argv[++i], "interlaced-pair");
            else throw std::runtime_error("unknown argument: " + arg);
        }
        if (interlaced_pair != 2) throw std::runtime_error("--interlaced-pair must be 2");
        std::filesystem::create_directories(out_dir);
        write_mixed(std::filesystem::path(out_dir) / "mixed_1080p.h264", frames, slices);
        write_paff(std::filesystem::path(out_dir) / "interlaced_paff.h264", 96);
        write_mbaff(std::filesystem::path(out_dir) / "interlaced_mbaff.h264", 64);
        write_progressive(std::filesystem::path(out_dir) / "long_gop.h264", 300, 2, true, 50);
        write_truncated(std::filesystem::path(out_dir) / "truncated.h264");
        std::cout << "generated " << out_dir << "/mixed_1080p.h264 frames=" << frames
                  << " slices=" << slices << '\n';
        std::cout << "generated " << out_dir << "/interlaced_paff.h264 frames=96 fields=2\n";
        std::cout << "generated " << out_dir << "/interlaced_mbaff.h264 frames=64\n";
        std::cout << "generated " << out_dir << "/long_gop.h264 frames=300\n";
        std::cout << "generated " << out_dir << "/truncated.h264 frames=30 truncated_tail=1\n";
        return 0;
    } catch (const std::exception& error) {
        std::cerr << "fixture_gen: " << error.what() << '\n';
        return 1;
    }
}
