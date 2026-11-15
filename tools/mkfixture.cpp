#include "fixture_boxes.h"

#include "error.h"

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <limits>
#include <set>
#include <sstream>
#include <string>
#include <vector>

namespace {

struct Options {
    std::filesystem::path output = "fixtures/a";
    uint32_t fragments = 12;
    std::set<uint32_t> drop;
    std::set<uint32_t> duplicate;
    std::string tfdt = "mixed";
    std::string trun = "mixed";
    uint64_t seed = 7;
    bool negative_offset = false;
    bool large_mdat = false;
    bool reorder = false;
    fixture::Config media;
};

uint64_t parseUnsigned(const std::string& text, const std::string& option) {
    if (text.empty()) throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "empty value for " + option);
    size_t used = 0;
    try {
        const unsigned long long value = std::stoull(text, &used, 10);
        if (used != text.size()) throw std::invalid_argument("trailing");
        return static_cast<uint64_t>(value);
    } catch (const std::exception&) {
        throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "invalid value for " + option + ": " + text);
    }
}

std::set<uint32_t> parseList(const std::string& text, const std::string& option) {
    std::set<uint32_t> values;
    if (text.empty()) return values;
    std::istringstream input(text);
    std::string item;
    while (std::getline(input, item, ',')) {
        const uint64_t value = parseUnsigned(item, option);
        if (value == 0 || value > std::numeric_limits<uint32_t>::max()) {
            throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "fragment sequence must be positive");
        }
        values.insert(static_cast<uint32_t>(value));
    }
    return values;
}

Options parseOptions(int argc, char** argv) {
    Options options;
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        auto value = [&](const std::string& name) -> std::string {
            if (i + 1 >= argc) {
                throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "missing value for " + name);
            }
            return argv[++i];
        };
        if (arg == "--out") options.output = value(arg);
        else if (arg == "--fragments") options.fragments = static_cast<uint32_t>(parseUnsigned(value(arg), arg));
        else if (arg == "--drop") options.drop = parseList(value(arg), arg);
        else if (arg == "--dup") options.duplicate = parseList(value(arg), arg);
        else if (arg == "--tfdt") options.tfdt = value(arg);
        else if (arg == "--trun") options.trun = value(arg);
        else if (arg == "--seed") options.seed = parseUnsigned(value(arg), arg);
        else if (arg == "--codec") options.media.codec = value(arg);
        else if (arg == "--timescale") options.media.timescale = static_cast<uint32_t>(parseUnsigned(value(arg), arg));
        else if (arg == "--default-duration") options.media.default_duration = static_cast<uint32_t>(parseUnsigned(value(arg), arg));
        else if (arg == "--samples") options.media.samples_per_fragment = static_cast<uint32_t>(parseUnsigned(value(arg), arg));
        else if (arg == "--negative-offset") options.negative_offset = true;
        else if (arg == "--large-mdat") options.large_mdat = true;
        else if (arg == "--reorder") options.reorder = true;
        else if (arg == "--help" || arg == "-h") {
            std::cout << "usage: fmp4gen --out DIR [--fragments N] [--drop 5,9] [--dup 3]\n"
                         "               [--tfdt mixed|0|1] [--trun mixed|0|1] [--seed N]\n"
                         "               [--codec avc1] [--timescale 90000]\n"
                         "               [--default-duration 60000] [--samples 3]\n"
                         "               [--negative-offset] [--large-mdat] [--reorder]\n";
            std::exit(0);
        } else {
            throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "unknown fmp4gen option: " + arg);
        }
    }
    if (options.fragments == 0 || options.media.timescale == 0 ||
        options.media.default_duration == 0 || options.media.samples_per_fragment == 0 ||
        options.media.codec.size() != 4) {
        throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "fixture parameters must be nonzero and codec must be fourcc");
    }
    if (options.tfdt != "mixed" && options.tfdt != "0" && options.tfdt != "1") {
        throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "--tfdt must be mixed, 0, or 1");
    }
    if (options.trun != "mixed" && options.trun != "0" && options.trun != "1") {
        throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "--trun must be mixed, 0, or 1");
    }
    if (options.negative_offset && options.trun == "0") {
        throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "negative data_offset requires trun version 1");
    }
    return options;
}

uint8_t chooseVersion(const std::string& mode, uint32_t sequence) {
    if (mode == "0") return 0;
    if (mode == "1") return 1;
    return sequence % 2 == 0 ? 1 : 0;
}

uint32_t jitterMilliseconds(uint64_t seed, uint32_t sequence) {
    uint64_t value = seed ^ (static_cast<uint64_t>(sequence) * 0x9E3779B97F4A7C15ULL);
    value ^= value >> 30;
    value *= 0xBF58476D1CE4E5B9ULL;
    value ^= value >> 27;
    return 5 + static_cast<uint32_t>(value % 26);
}

void writeBytes(std::ostream& out, const std::vector<uint8_t>& bytes) {
    out.write(reinterpret_cast<const char*>(bytes.data()), static_cast<std::streamsize>(bytes.size()));
    if (!out) throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "failed to write fixture");
}

void writeFragment(std::ostream& out, const Options& options, uint32_t sequence,
                   uint32_t timeline_index, uint64_t& offset) {
    const uint8_t tfdt_version = chooseVersion(options.tfdt, sequence);
    const uint8_t trun_version = options.negative_offset ? 1 : chooseVersion(options.trun, sequence);
    const uint64_t payload_size = fixture::fragmentPayloadSize(options.media);
    const uint64_t mdat_header_size = options.large_mdat ? 16 : 8;
    const uint64_t fragment_ticks = static_cast<uint64_t>(options.media.default_duration) *
                                    options.media.samples_per_fragment;
    const uint64_t jitter_ticks = static_cast<uint64_t>(jitterMilliseconds(options.seed, timeline_index)) *
                                  options.media.timescale / 1000;
    const uint64_t tfdt =
        static_cast<uint64_t>(timeline_index - 1) * fragment_ticks + jitter_ticks;
    if (tfdt_version == 0 && tfdt > std::numeric_limits<uint32_t>::max()) {
        throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "tfdt v0 overflow; use --tfdt 1");
    }

    const fixture::Bytes moof = fixture::buildMoof(
        options.media, sequence, tfdt, tfdt_version, trun_version, options.negative_offset,
        offset, mdat_header_size);
    writeBytes(out, moof);
    offset += moof.size();
    const fixture::Bytes mdat_header = fixture::buildMdatHeader(options.media, payload_size,
                                                                options.large_mdat);
    writeBytes(out, mdat_header);
    offset += mdat_header.size();

    std::vector<uint8_t> payload(static_cast<size_t>(payload_size));
    uint64_t state = options.seed ^ (static_cast<uint64_t>(sequence) << 32) ^ offset;
    for (uint8_t& byte : payload) {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        byte = static_cast<uint8_t>(state);
    }
    writeBytes(out, payload);
    offset += payload.size();
}

std::filesystem::path resolveOutput(const std::filesystem::path& requested) {
    if (requested.has_extension()) return requested;
    return requested / "input.mp4";
}

}  // namespace

int main(int argc, char** argv) {
    try {
        const Options options = parseOptions(argc, argv);
        int64_t base_count = static_cast<int64_t>(options.fragments) +
                             static_cast<int64_t>(options.drop.size()) -
                             static_cast<int64_t>(options.duplicate.size());
        if (base_count < 1) {
            throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "defect counts produce no fragments");
        }
        const std::filesystem::path output = resolveOutput(options.output);
        if (!output.parent_path().empty()) {
            std::filesystem::create_directories(output.parent_path());
        }
        std::ofstream file(output, std::ios::binary | std::ios::trunc);
        if (!file) {
            throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "cannot create fixture: " + output.string());
        }
        uint64_t offset = 0;
        const fixture::Bytes init = fixture::buildInit(options.media);
        writeBytes(file, init);
        offset += init.size();
        std::vector<uint32_t> emitted_sequences;
        for (uint32_t sequence = 1; sequence <= static_cast<uint32_t>(base_count); ++sequence) {
            if (options.drop.count(sequence) == 0) {
                emitted_sequences.push_back(sequence);
            }
            if (options.duplicate.count(sequence) != 0) {
                emitted_sequences.push_back(sequence);
            }
        }
        if (options.reorder && emitted_sequences.size() > 1) {
            std::swap(emitted_sequences[0], emitted_sequences[1]);
        }
        uint32_t timeline_index = 1;
        for (uint32_t sequence : emitted_sequences) {
            writeFragment(file, options, sequence, timeline_index++, offset);
        }
        const uint32_t emitted = static_cast<uint32_t>(emitted_sequences.size());
        file.flush();
        if (!file) throw fmp4::Error(fmp4::ExitCode::UsageOrIo, "failed to flush fixture");
        if (emitted != options.fragments) {
            throw fmp4::Error(fmp4::ExitCode::UsageOrIo,
                              "requested " + std::to_string(options.fragments) + " fragments but emitted " +
                                  std::to_string(emitted) + "; defect sequences are out of range");
        }
        std::cout << "wrote " << output << " fragments=" << emitted << " bytes=" << offset << '\n';
        return 0;
    } catch (const fmp4::Error& error) {
        std::cerr << "fmp4gen: " << error.what() << '\n';
        return static_cast<int>(error.code());
    } catch (const std::exception& error) {
        std::cerr << "fmp4gen: " << error.what() << '\n';
        return static_cast<int>(fmp4::ExitCode::UsageOrIo);
    }
}

