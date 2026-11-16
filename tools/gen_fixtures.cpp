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
    void bit(bool value) {
        if (bitPosition_ == 0) {
            data_.push_back(0);
        }
        if (value) {
            data_.back() |= static_cast<std::uint8_t>(0x80U >> bitPosition_);
        }
        bitPosition_ = (bitPosition_ + 1) % 8;
    }

    void bits(std::uint64_t value, unsigned count) {
        for (unsigned i = 0; i < count; ++i) {
            bit(((value >> (count - 1 - i)) & 1U) != 0);
        }
    }

    void ue(std::uint32_t codeNum) {
        const std::uint64_t value = static_cast<std::uint64_t>(codeNum) + 1;
        unsigned length = 0;
        for (std::uint64_t temp = value; temp != 0; temp >>= 1) {
            ++length;
        }
        for (unsigned i = 1; i < length; ++i) {
            bit(false);
        }
        bits(value, length);
    }

    void se(std::int32_t value) {
        const std::uint32_t codeNum = value > 0
            ? static_cast<std::uint32_t>(value) * 2U - 1U
            : static_cast<std::uint32_t>(-static_cast<std::int64_t>(value)) * 2U;
        ue(codeNum);
    }

    void rbspTrailingBits() {
        bit(true);
        while (bitPosition_ != 0) {
            bit(false);
        }
    }

    std::vector<std::uint8_t> take() { return std::move(data_); }

private:
    std::vector<std::uint8_t> data_;
    unsigned bitPosition_ = 0;
};

struct SpsConfig {
    std::uint32_t id = 0;
    std::uint32_t profileIdc = 100;
    std::uint32_t levelIdc = 40;
    std::uint32_t log2MaxFrameNum = 4;
    std::uint32_t log2MaxPocLsb = 8;
    std::uint32_t maxNumRefFrames = 2;
    std::uint32_t widthMbs = 80;
    std::uint32_t heightMapUnits = 45;
    bool frameMbsOnly = true;
    bool mbaff = false;
    bool crop = false;
    std::uint32_t cropLeft = 0;
    std::uint32_t cropRight = 0;
    std::uint32_t cropTop = 0;
    std::uint32_t cropBottom = 0;
};

struct PpsConfig {
    std::uint32_t id = 0;
    std::uint32_t spsId = 0;
    bool entropyCabac = true;
    std::uint32_t numSliceGroups = 1;
    std::uint32_t fmoMapType = 0;
    bool weightedPred = true;
    std::uint32_t weightedBipredIdc = 1;
    std::int32_t picInitQpMinus26 = 0;
};

bool isHighProfile(std::uint32_t profileIdc) {
    switch (profileIdc) {
        case 100: case 110: case 122: case 244: case 44:
        case 83: case 86: case 118: case 128: case 138:
        case 139: case 134:
            return true;
        default:
            return false;
    }
}

std::vector<std::uint8_t> buildSps(const SpsConfig& config) {
    BitWriter writer;
    writer.bits(config.profileIdc, 8);
    writer.bits(0, 8);
    writer.bits(config.levelIdc, 8);
    writer.ue(config.id);
    if (isHighProfile(config.profileIdc)) {
        writer.ue(1);
        writer.ue(0);
        writer.ue(0);
        writer.bit(false);
        writer.bit(false);
    }
    writer.ue(config.log2MaxFrameNum - 4);
    writer.ue(0);
    writer.ue(config.log2MaxPocLsb - 4);
    writer.ue(config.maxNumRefFrames);
    writer.bit(false);
    writer.ue(config.widthMbs - 1);
    writer.ue(config.heightMapUnits - 1);
    writer.bit(config.frameMbsOnly);
    if (!config.frameMbsOnly) {
        writer.bit(config.mbaff);
    }
    writer.bit(true);
    writer.bit(config.crop);
    if (config.crop) {
        writer.ue(config.cropLeft);
        writer.ue(config.cropRight);
        writer.ue(config.cropTop);
        writer.ue(config.cropBottom);
    }
    writer.bit(false);
    writer.rbspTrailingBits();
    return writer.take();
}

std::vector<std::uint8_t> buildPps(const PpsConfig& config) {
    BitWriter writer;
    writer.ue(config.id);
    writer.ue(config.spsId);
    writer.bit(config.entropyCabac);
    writer.bit(false);
    writer.ue(config.numSliceGroups - 1);
    if (config.numSliceGroups > 1) {
        writer.ue(config.fmoMapType);
        if (config.fmoMapType == 0) {
            for (std::uint32_t i = 0; i < config.numSliceGroups; ++i) {
                writer.ue(9);
            }
        } else if (config.fmoMapType == 2) {
            for (std::uint32_t i = 0; i + 1 < config.numSliceGroups; ++i) {
                writer.ue(i * 20);
                writer.ue(i * 20 + 9);
            }
        } else if (config.fmoMapType >= 3 && config.fmoMapType <= 5) {
            writer.bit(false);
            writer.ue(19);
        } else if (config.fmoMapType == 6) {
            const std::uint32_t mapUnits = 20;
            writer.ue(mapUnits - 1);
            const unsigned bits = config.numSliceGroups > 2 ? 2 : 1;
            for (std::uint32_t i = 0; i < mapUnits; ++i) {
                writer.bits(i % config.numSliceGroups, bits);
            }
        }
    }
    writer.ue(0);
    writer.ue(0);
    writer.bit(config.weightedPred);
    writer.bits(config.weightedBipredIdc, 2);
    writer.se(config.picInitQpMinus26);
    writer.se(0);
    writer.se(0);
    writer.bit(true);
    writer.bit(false);
    writer.bit(false);
    writer.rbspTrailingBits();
    return writer.take();
}

std::vector<std::uint8_t> escapeRbsp(const std::vector<std::uint8_t>& rbsp) {
    std::vector<std::uint8_t> escaped;
    escaped.reserve(rbsp.size() + 8);
    unsigned zeroCount = 0;
    for (const std::uint8_t byte : rbsp) {
        if (zeroCount >= 2 && byte <= 0x03) {
            escaped.push_back(0x03);
            zeroCount = 0;
        }
        escaped.push_back(byte);
        zeroCount = byte == 0 ? zeroCount + 1 : 0;
    }
    return escaped;
}

class StreamWriter {
public:
    void nal(std::uint8_t refIdc, std::uint8_t type,
             const std::vector<std::uint8_t>& rbsp, bool fourByte) {
        if (fourByte) {
            bytes_.push_back(0);
        }
        bytes_.push_back(0);
        bytes_.push_back(0);
        bytes_.push_back(1);
        bytes_.push_back(static_cast<std::uint8_t>((refIdc << 5) | type));
        const std::vector<std::uint8_t> escaped = escapeRbsp(rbsp);
        bytes_.insert(bytes_.end(), escaped.begin(), escaped.end());
    }

    const std::vector<std::uint8_t>& bytes() const { return bytes_; }

private:
    std::vector<std::uint8_t> bytes_;
};

enum class StartMode { ThreeByte, FourByte, Alternate };

class FixtureWriter {
public:
    explicit FixtureWriter(StartMode mode) : mode_(mode) {}

    void nal(std::uint8_t refIdc, std::uint8_t type,
             const std::vector<std::uint8_t>& rbsp) {
        bool fourByte = mode_ == StartMode::FourByte;
        if (mode_ == StartMode::Alternate) {
            fourByte = (index_ % 2) == 0;
        }
        stream_.nal(refIdc, type, rbsp, fourByte);
        ++index_;
    }

    const std::vector<std::uint8_t>& bytes() const { return stream_.bytes(); }

private:
    StreamWriter stream_;
    StartMode mode_;
    unsigned index_ = 0;
};

struct SliceConfig {
    std::uint32_t firstMb = 0;
    std::uint32_t sliceType = 2;
    std::uint32_t frameNum = 0;
    std::uint32_t pocLsb = 0;
    std::uint32_t idrPicId = 0;
    bool idr = false;
    bool field = false;
    bool bottom = false;
};

std::vector<std::uint8_t> buildSlice(const SpsConfig& sps, const PpsConfig& pps,
                                     const SliceConfig& slice) {
    BitWriter writer;
    writer.ue(slice.firstMb);
    writer.ue(slice.sliceType);
    writer.ue(pps.id);
    writer.bits(slice.frameNum, sps.log2MaxFrameNum);
    if (!sps.frameMbsOnly) {
        writer.bit(slice.field);
        if (slice.field) {
            writer.bit(slice.bottom);
        }
    }
    if (slice.idr) {
        writer.ue(slice.idrPicId);
    }
    writer.bits(slice.pocLsb, sps.log2MaxPocLsb);
    writer.bits(0x80, 8);
    writer.rbspTrailingBits();
    return writer.take();
}

std::vector<std::uint8_t> buildAud() {
    BitWriter writer;
    writer.bits(7, 3);
    writer.rbspTrailingBits();
    return writer.take();
}

void writeHeaders(FixtureWriter& output, const SpsConfig& sps, const PpsConfig& pps) {
    output.nal(3, 7, buildSps(sps));
    output.nal(3, 8, buildPps(pps));
}

void writePicture(FixtureWriter& output, const SpsConfig& sps, const PpsConfig& pps,
                  std::uint32_t sliceType, std::uint32_t frameNum,
                  std::uint32_t pocLsb, bool idr, bool field, bool bottom,
                  const std::vector<std::uint32_t>& firstMbs) {
    const std::uint8_t refIdc = idr ? 3 : (sliceType == 1 ? 0 : 2);
    for (const std::uint32_t firstMb : firstMbs) {
        SliceConfig slice;
        slice.firstMb = firstMb;
        slice.sliceType = sliceType;
        slice.frameNum = frameNum;
        slice.pocLsb = pocLsb;
        slice.idrPicId = 0;
        slice.idr = idr;
        slice.field = field;
        slice.bottom = bottom;
        output.nal(refIdc, idr ? 5 : 1, buildSlice(sps, pps, slice));
    }
}

void saveFixture(const std::filesystem::path& directory, const std::string& name,
                 const FixtureWriter& fixture) {
    const std::filesystem::path path = directory / name;
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    if (!output) {
        throw std::runtime_error("cannot create fixture: " + path.string());
    }
    const std::vector<std::uint8_t>& bytes = fixture.bytes();
    output.write(reinterpret_cast<const char*>(bytes.data()),
                 static_cast<std::streamsize>(bytes.size()));
    if (!output) {
        throw std::runtime_error("cannot write fixture: " + path.string());
    }
}

void generateBaseline(const std::filesystem::path& directory, const std::string& name,
                      StartMode mode) {
    SpsConfig sps;
    PpsConfig pps;
    FixtureWriter fixture(mode);
    writeHeaders(fixture, sps, pps);
    const std::uint32_t types[8] = {2, 0, 1, 0, 1, 0, 1, 1};
    for (std::uint32_t i = 0; i < 8; ++i) {
        writePicture(fixture, sps, pps, types[i], i, i * 2, i == 0, false, false, {0});
    }
    saveFixture(directory, name, fixture);
}

void generateMultislice(const std::filesystem::path& directory) {
    SpsConfig sps;
    sps.widthMbs = 80;
    PpsConfig pps;
    pps.numSliceGroups = 2;
    pps.fmoMapType = 2;
    pps.picInitQpMinus26 = -2;
    FixtureWriter fixture(StartMode::Alternate);
    writeHeaders(fixture, sps, pps);
    for (std::uint32_t i = 0; i < 15; ++i) {
        const std::uint32_t type = i == 0 ? 2 : (i % 3 == 1 ? 0 : 1);
        std::vector<std::uint32_t> firstMbs = {0};
        if (i == 3 || i == 7 || i == 11) {
            firstMbs.push_back(40);
        }
        writePicture(fixture, sps, pps, type, i, i * 2, i == 0, false, false, firstMbs);
    }
    saveFixture(directory, "multislice.h264", fixture);
}

void writeThirtyFrameGop(FixtureWriter& fixture, const SpsConfig& sps,
                         const PpsConfig& pps, std::uint32_t gopIndex,
                         bool multiSlice) {
    for (std::uint32_t i = 0; i < 30; ++i) {
        std::uint32_t type = 1;
        if (i == 0) {
            type = 2;
        } else if (i <= 26 && (i % 2) == 1) {
            type = 0;
        }
        std::vector<std::uint32_t> firstMbs = {0};
        if (multiSlice &&
            (i == 2 || i == 9 || i == 16 || i == 23)) {
            firstMbs.push_back(40);
        }
        if (multiSlice && gopIndex == 3 && i == 29) {
            firstMbs.push_back(20);
        }
        if (i == 0 && gopIndex == 1) {
            fixture.nal(0, 9, buildAud());
        }
        writePicture(fixture, sps, pps, type, i, i * 2, i == 0, false, false, firstMbs);
    }
}

void generateMbaff(const std::filesystem::path& directory) {
    SpsConfig sps;
    sps.frameMbsOnly = false;
    sps.mbaff = true;
    sps.crop = true;
    sps.cropTop = 180;
    sps.cropBottom = 180;
    PpsConfig pps;
    FixtureWriter fixture(StartMode::Alternate);
    writeHeaders(fixture, sps, pps);
    for (std::uint32_t gop = 0; gop < 4; ++gop) {
        writeThirtyFrameGop(fixture, sps, pps, gop, true);
    }
    saveFixture(directory, "mbaff_multi_slice.h264", fixture);
}

void generateInterlacedMix(const std::filesystem::path& directory) {
    SpsConfig sps;
    sps.frameMbsOnly = false;
    sps.mbaff = false;
    PpsConfig pps;
    FixtureWriter fixture(StartMode::Alternate);
    writeHeaders(fixture, sps, pps);
    for (std::uint32_t frame = 0; frame < 12; ++frame) {
        const std::uint32_t type = frame == 0 ? 2 : (frame % 2 == 1 ? 0 : 1);
        writePicture(fixture, sps, pps, type, frame, frame * 2, frame == 0,
                     true, false, {0});
        writePicture(fixture, sps, pps, type, frame, frame * 2 + 1, frame == 0,
                     true, true, {0});
        if (frame == 5) {
            fixture.nal(0, 9, buildAud());
        }
    }
    saveFixture(directory, "interlaced_mix.h264", fixture);
}

void generateSpsChange(const std::filesystem::path& directory) {
    SpsConfig first;
    PpsConfig pps;
    FixtureWriter fixture(StartMode::Alternate);
    writeHeaders(fixture, first, pps);
    for (std::uint32_t i = 0; i < 8; ++i) {
        const std::uint32_t type = i == 0 ? 2 : (i % 2 == 1 ? 0 : 1);
        writePicture(fixture, first, pps, type, i, i * 2, i == 0, false, false, {0});
    }

    SpsConfig second = first;
    second.widthMbs = 64;
    second.levelIdc = 31;
    writeHeaders(fixture, second, pps);
    for (std::uint32_t i = 0; i < 10; ++i) {
        const std::uint32_t type = i == 0 ? 2 : (i % 2 == 1 ? 0 : 1);
        writePicture(fixture, second, pps, type, i, i * 2, i == 0, false, false, {0});
    }
    saveFixture(directory, "sps_change.h264", fixture);
}

}  // namespace

int main(int argc, char** argv) {
    if (argc != 5 || std::string(argv[1]) != "--out" || std::string(argv[3]) != "--cases") {
        std::cerr << "usage: gen_fixtures --out <directory> --cases <count>\n";
        return 3;
    }
    try {
        const std::filesystem::path output(argv[2]);
        const int cases = std::stoi(argv[4]);
        if (cases < 0 || cases > 6) {
            throw std::runtime_error("--cases must be between 0 and 6");
        }
        std::filesystem::create_directories(output);
        if (cases >= 1) generateBaseline(output, "baseline_3byte.h264", StartMode::ThreeByte);
        if (cases >= 2) generateBaseline(output, "baseline_4byte.h264", StartMode::FourByte);
        if (cases >= 3) generateMultislice(output);
        if (cases >= 4) generateMbaff(output);
        if (cases >= 5) generateInterlacedMix(output);
        if (cases >= 6) generateSpsChange(output);
        std::cout << "generated cases=" << cases << " out=" << output.string() << '\n';
    } catch (const std::exception& error) {
        std::cerr << "error: " << error.what() << '\n';
        return 1;
    }
    return 0;
}
