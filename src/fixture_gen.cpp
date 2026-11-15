#include "fixture_gen.h"

#include <cstddef>
#include <filesystem>
#include <fstream>
#include <limits>
#include <string>
#include <vector>

namespace h264scan {
namespace {

constexpr uint32_t kFramesPerGop = 24;
constexpr uint32_t kWidthMbs = 80;
constexpr uint32_t kHeightMapUnits = 23;

class BitWriter {
public:
    void writeBit(bool bit) {
        if ((bit_count_ % 8U) == 0U) {
            bytes_.push_back(0);
        }
        if (bit) {
            bytes_.back() |= static_cast<uint8_t>(1U << (7U - (bit_count_ % 8U)));
        }
        ++bit_count_;
    }

    void writeBits(uint32_t value, unsigned int count) {
        for (unsigned int i = 0; i < count; ++i) {
            const unsigned int shift = count - 1U - i;
            writeBit(((value >> shift) & 1U) != 0U);
        }
    }

    void writeUE(uint32_t value) {
        const uint64_t value_plus_one = static_cast<uint64_t>(value) + 1U;
        unsigned int suffix_bits = 0;
        while ((uint64_t{1} << (suffix_bits + 1U)) <= value_plus_one) {
            ++suffix_bits;
        }
        for (unsigned int i = 0; i < suffix_bits; ++i) {
            writeBit(false);
        }
        writeBits(static_cast<uint32_t>(value_plus_one), suffix_bits + 1U);
    }

    void writeSE(int32_t value) {
        const int64_t signed_value = value;
        const uint32_t code_num = signed_value <= 0
                                      ? static_cast<uint32_t>(-2 * signed_value)
                                      : static_cast<uint32_t>(2 * signed_value - 1);
        writeUE(code_num);
    }

    void alignZero() {
        while ((bit_count_ % 8U) != 0U) {
            writeBit(false);
        }
    }

    void writeTrailingBits() {
        writeBit(true);
        alignZero();
    }

    const std::vector<uint8_t>& bytes() const { return bytes_; }

private:
    std::vector<uint8_t> bytes_;
    size_t bit_count_ = 0;
};

class Random {
public:
    explicit Random(uint64_t seed) : state_(seed == 0U ? 0x9e3779b97f4a7c15ULL : seed) {}

    uint64_t next() {
        state_ ^= state_ >> 12U;
        state_ ^= state_ << 25U;
        state_ ^= state_ >> 27U;
        return state_ * 2685821657736338717ULL;
    }

    uint8_t byte() { return static_cast<uint8_t>(next() >> 56U); }

private:
    uint64_t state_;
};

std::vector<uint8_t> escapeRbsp(const std::vector<uint8_t>& rbsp) {
    std::vector<uint8_t> ebsp;
    ebsp.reserve(rbsp.size() + rbsp.size() / 32U + 4U);
    unsigned int zero_count = 0;
    for (uint8_t byte : rbsp) {
        if (zero_count >= 2U && byte <= 0x03U) {
            ebsp.push_back(0x03U);
            zero_count = 0;
        }
        ebsp.push_back(byte);
        zero_count = byte == 0U ? zero_count + 1U : 0U;
    }
    return ebsp;
}

std::vector<uint8_t> makeFillerPayload(Random& random, size_t size) {
    std::vector<uint8_t> payload;
    payload.reserve(size);
    for (size_t i = 0; i < size; ++i) {
        if ((i % 97U) == 0U) {
            payload.push_back(0x00U);
            payload.push_back(0x00U);
            payload.push_back(0x01U);
        } else {
            payload.push_back(random.byte());
        }
    }
    return payload;
}

class NalWriter {
public:
    explicit NalWriter(std::ofstream& output) : output_(output) {}

    bool write(uint8_t type, uint8_t ref_idc, const std::vector<uint8_t>& rbsp,
               bool long_start_code, unsigned int trailing_zeros,
               uint64_t& start_offset, std::string& error) {
        const std::vector<uint8_t> ebsp = escapeRbsp(rbsp);
        start_offset = offset_;
        if (long_start_code) {
            writeByte(0x00U);
            writeByte(0x00U);
            writeByte(0x00U);
        } else {
            writeByte(0x00U);
            writeByte(0x00U);
        }
        writeByte(0x01U);
        writeByte(static_cast<uint8_t>((ref_idc << 5U) | (type & 0x1fU)));
        for (uint8_t byte : ebsp) {
            writeByte(byte);
        }
        for (unsigned int i = 0; i < trailing_zeros; ++i) {
            writeByte(0x00U);
        }
        if (!output_) {
            error = "failed while writing synthetic stream";
            return false;
        }
        return true;
    }

    uint64_t offset() const { return offset_; }

private:
    void writeByte(uint8_t byte) {
        output_.put(static_cast<char>(byte));
        ++offset_;
    }

    std::ofstream& output_;
    uint64_t offset_ = 0;
};

std::vector<uint8_t> makeSps() {
    BitWriter writer;
    writer.writeBits(100U, 8U);
    writer.writeBits(0U, 8U);
    writer.writeBits(41U, 8U);
    writer.writeUE(0U);
    writer.writeUE(1U);
    writer.writeUE(0U);
    writer.writeUE(0U);
    writer.writeBit(false);
    writer.writeBit(true);
    for (unsigned int list = 0; list < 8U; ++list) {
        writer.writeBit(list == 0U);
        if (list == 0U) {
            for (unsigned int i = 0; i < 16U; ++i) {
                writer.writeSE(0);
            }
        }
    }
    writer.writeUE(0U);
    writer.writeUE(0U);
    writer.writeUE(2U);
    writer.writeUE(3U);
    writer.writeBit(false);
    writer.writeUE(kWidthMbs - 1U);
    writer.writeUE(kHeightMapUnits - 1U);
    writer.writeBit(false);
    writer.writeBit(false);
    writer.writeBit(true);
    writer.writeBit(true);
    writer.writeUE(0U);
    writer.writeUE(0U);
    writer.writeUE(0U);
    writer.writeUE(4U);
    writer.writeBit(false);
    writer.writeTrailingBits();
    return writer.bytes();
}

std::vector<uint8_t> makePps() {
    BitWriter writer;
    writer.writeUE(0U);
    writer.writeUE(0U);
    writer.writeBit(false);
    writer.writeBit(false);
    writer.writeUE(0U);
    writer.writeUE(0U);
    writer.writeUE(0U);
    writer.writeBit(false);
    writer.writeBits(0U, 2U);
    writer.writeSE(0);
    writer.writeSE(0);
    writer.writeSE(0);
    writer.writeBit(false);
    writer.writeBit(false);
    writer.writeBit(false);
    writer.writeTrailingBits();
    return writer.bytes();
}

uint32_t sliceTypeForFrame(uint32_t frame_index) {
    if (frame_index == 0U) {
        return 2U;
    }
    return (frame_index % 3U) == 1U ? 0U : 1U;
}

}  // namespace

bool validateSynthOptions(const SynthOptions& options, std::string& error) {
    error.clear();
    if (options.output_path.empty()) {
        error = "--out is required";
    } else if (options.gops == 0U || options.gops > 100000U) {
        error = "--gops must be between 1 and 100000";
    } else if (options.slices == 0U || options.slices > 64U) {
        error = "--slices must be between 1 and 64";
    } else if (options.interlaced_pair && options.slices < 2U) {
        error = "pair field mode requires at least 2 slices";
    }
    return error.empty();
}

bool generateFixture(const SynthOptions& options, std::string& error) {
    if (!validateSynthOptions(options, error)) {
        return false;
    }

    const std::filesystem::path output_path(options.output_path);
    const std::filesystem::path parent = output_path.parent_path();
    if (!parent.empty()) {
        std::error_code filesystem_error;
        std::filesystem::create_directories(parent, filesystem_error);
        if (filesystem_error) {
            error = "cannot create output directory: " + filesystem_error.message();
            return false;
        }
    }

    std::ofstream output(output_path, std::ios::binary | std::ios::trunc);
    if (!output) {
        error = "cannot open synthetic output: " + options.output_path;
        return false;
    }

    NalWriter writer(output);
    Random random(options.seed);
    uint64_t ignored_offset = 0;

    const std::vector<uint8_t> filler = makeFillerPayload(random, 12000U);
    if (!writer.write(12U, 0U, filler, false, 1U, ignored_offset, error)) {
        return false;
    }
    if (!writer.write(7U, 3U, makeSps(), true, 0U, ignored_offset, error) ||
        !writer.write(8U, 3U, makePps(), true, 0U, ignored_offset, error)) {
        return false;
    }

    const uint32_t mbs_in_frame = kWidthMbs * kHeightMapUnits * 2U;
    for (uint32_t gop = 0; gop < options.gops; ++gop) {
        for (uint32_t frame = 0; frame < kFramesPerGop; ++frame) {
            const bool idr = frame == 0U;
            const bool field_coded = options.interlaced_pair && (frame % 2U == 1U);
            const uint32_t slice_type = sliceTypeForFrame(frame);
            const uint8_t ref_idc = slice_type == 1U ? 2U : 3U;
            const uint32_t frame_num = frame % 16U;
            uint32_t poc_lsb = frame * 2U;
            const uint32_t idr_pic_id = gop % 65536U;
            const uint32_t top_slices = (options.slices + 1U) / 2U;

            for (uint32_t slice_index = 0; slice_index < options.slices; ++slice_index) {
                bool field_pic = false;
                bool bottom_field = false;
                uint32_t first_mb = 0;
                if (field_coded) {
                    field_pic = true;
                    bottom_field = slice_index >= top_slices;
                    first_mb = bottom_field ? slice_index - top_slices : slice_index;
                    if (bottom_field) {
                        ++poc_lsb;
                    }
                } else {
                    first_mb = slice_index * (mbs_in_frame / options.slices);
                }

                BitWriter header;
                header.writeUE(first_mb);
                header.writeUE(slice_type);
                header.writeUE(0U);
                header.writeBits(frame_num, 4U);
                header.writeBit(field_pic);
                if (field_pic) {
                    header.writeBit(bottom_field);
                }
                if (idr) {
                    header.writeUE(idr_pic_id);
                }
                header.writeBits(poc_lsb, 6U);
                header.writeTrailingBits();

                std::vector<uint8_t> rbsp = header.bytes();
                const std::vector<uint8_t> padding =
                    makeFillerPayload(random, 320U + static_cast<size_t>(random.next() % 180U));
                rbsp.insert(rbsp.end(), padding.begin(), padding.end());

                const uint8_t nal_type = idr ? 5U : 1U;
                const unsigned int trailing_zeros =
                    ((gop + frame + slice_index) % 7U == 0U) ? 1U : 0U;
                if (!writer.write(nal_type, ref_idc, rbsp, idr, trailing_zeros,
                                  ignored_offset, error)) {
                    return false;
                }
            }
        }
    }

    output.flush();
    if (!output) {
        error = "failed to flush synthetic output";
        return false;
    }
    return true;
}

}  // namespace h264scan
