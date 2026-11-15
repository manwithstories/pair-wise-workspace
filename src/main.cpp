#include "box_tree.h"
#include "compare.h"
#include "error.h"
#include "fragment_index.h"
#include "init_segment.h"
#include "repack.h"
#include "report.h"
#include "validate.h"

#include <filesystem>
#include <fstream>
#include <iostream>
#include <sstream>
#include <string>
#include <vector>

namespace {

using fmp4::BoxReader;
using fmp4::BoxTree;
using fmp4::Error;
using fmp4::ExitCode;

std::string readTextFile(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) throw fmp4::ioError("cannot open file: " + path);
    std::ostringstream content;
    content << input.rdbuf();
    if (!input.good() && !input.eof()) throw fmp4::ioError("cannot read file: " + path);
    return content.str();
}

void writeTextFile(const std::string& path, const std::string& content) {
    if (path == "-") {
        std::cout << content;
        if (content.empty() || content.back() != '\n') std::cout << '\n';
        return;
    }
    const std::filesystem::path destination(path);
    if (!destination.parent_path().empty()) {
        std::filesystem::create_directories(destination.parent_path());
    }
    std::ofstream output(destination, std::ios::binary | std::ios::trunc);
    if (!output) throw fmp4::ioError("cannot create output file: " + path);
    output << content;
    if (!content.empty() && content.back() != '\n') output << '\n';
    output.flush();
    if (!output) throw fmp4::ioError("failed to write output file: " + path);
}

bool looksLikeJson(const std::string& path) {
    std::ifstream input(path);
    if (!input) throw fmp4::ioError("cannot open file: " + path);
    char c = '\0';
    while (input.get(c)) {
        if (c != ' ' && c != '\t' && c != '\r' && c != '\n') return c == '{';
    }
    return false;
}

std::string mismatchJson(const std::vector<fmp4::InitMismatch>& diffs) {
    std::ostringstream out;
    out << '[';
    for (size_t i = 0; i < diffs.size(); ++i) {
        if (i) out << ',';
        auto quote = [](const std::string& value) {
            std::string result = "\"";
            for (char c : value) {
                if (c == '"' || c == '\\') result.push_back('\\');
                result.push_back(c);
            }
            result.push_back('"');
            return result;
        };
        out << "{\"field\":" << quote(diffs[i].field) << ",\"left\":" << quote(diffs[i].left)
            << ",\"right\":" << quote(diffs[i].right) << '}';
    }
    out << ']';
    return out.str();
}

int runProbe(int argc, char** argv) {
    if (argc < 3) throw Error(ExitCode::UsageOrIo, "usage: fmp4tool probe FILE [--json PATH]");
    std::string json_path;
    for (int i = 3; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg == "--json" && i + 1 < argc) json_path = argv[++i];
        else throw Error(ExitCode::UsageOrIo, "unknown probe option: " + arg);
    }
    BoxReader reader(argv[2]);
    const BoxTree tree = BoxTree::parse(reader);
    const fmp4::InitSegment init = fmp4::InitSegment::parse(reader, tree);
    const fmp4::FragmentIndex index = fmp4::FragmentIndex::parse(reader, tree, &init);
    const std::string json = fmp4::probeJson(init, index);
    if (!json_path.empty()) writeTextFile(json_path, json);
    std::cout << (json_path.empty() ? fmp4::probeText(init, index) : json + "\n");
    return 0;
}

int runValidate(int argc, char** argv) {
    if (argc < 3) {
        throw Error(ExitCode::UsageOrIo,
                    "usage: fmp4tool validate FILE [--json PATH] [--init INIT_FILE]");
    }
    std::string json_path;
    std::string init_path;
    for (int i = 3; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg == "--json" && i + 1 < argc) json_path = argv[++i];
        else if (arg == "--init" && i + 1 < argc) init_path = argv[++i];
        else throw Error(ExitCode::UsageOrIo, "unknown validate option: " + arg);
    }
    BoxReader reader(argv[2]);
    const BoxTree tree = BoxTree::parse(reader);
    fmp4::InitSegment expected;
    bool has_expected = false;
    if (!init_path.empty()) {
        BoxReader init_reader(init_path);
        const BoxTree init_tree = BoxTree::parse(init_reader);
        expected = fmp4::InitSegment::parse(init_reader, init_tree);
        has_expected = true;
    }
    const fmp4::ValidationResult result =
        fmp4::validateMedia(reader, tree, has_expected ? &expected : nullptr);
    const std::string json = fmp4::validationJson(result);
    if (!json_path.empty()) writeTextFile(json_path, json);
    std::cout << (json_path.empty() ? fmp4::validationText(result) : json + "\n");
    return result.passed ? 0 : 1;
}

int runRepack(int argc, char** argv) {
    if (argc < 4) {
        throw Error(ExitCode::UsageOrIo,
                    "usage: fmp4tool repack INPUT -o OUTPUT [--tfdt-version 1] [--renumber]");
    }
    std::string output;
    fmp4::RepackOptions options;
    for (int i = 3; i < argc; ++i) {
        const std::string arg = argv[i];
        if ((arg == "-o" || arg == "--output") && i + 1 < argc) output = argv[++i];
        else if (arg == "--renumber") options.renumber = true;
        else if (arg == "--tfdt-version" && i + 1 < argc) {
            const std::string version = argv[++i];
            if (version != "1") throw Error(ExitCode::UsageOrIo, "only tfdt version 1 is supported");
            options.tfdt_version = 1;
        } else {
            throw Error(ExitCode::UsageOrIo, "unknown repack option: " + arg);
        }
    }
    if (output.empty()) throw Error(ExitCode::UsageOrIo, "repack requires -o OUTPUT");
    const fmp4::RepackStats stats = fmp4::repackFile(argv[2], output, options);
    std::cout << "repacked fragments=" << stats.fragment_count << " output=" << output << '\n';
    return 0;
}

int runCompare(int argc, char** argv) {
    if (argc < 4) throw Error(ExitCode::UsageOrIo, "usage: fmp4tool compare LEFT RIGHT [--json PATH]");
    std::string json_path;
    for (int i = 4; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg == "--json" && i + 1 < argc) json_path = argv[++i];
        else throw Error(ExitCode::UsageOrIo, "unknown compare option: " + arg);
    }
    bool passed = false;
    if (looksLikeJson(argv[2]) && looksLikeJson(argv[3])) {
        fmp4::CompareOutput comparison =
            fmp4::compareValidationJson(readTextFile(argv[2]), readTextFile(argv[3]));
        if (!json_path.empty()) writeTextFile(json_path, comparison.json);
        std::cout << comparison.json << '\n';
        passed = comparison.passed;
    } else {
        BoxReader left_reader(argv[2]);
        BoxReader right_reader(argv[3]);
        const BoxTree left_tree = BoxTree::parse(left_reader);
        const BoxTree right_tree = BoxTree::parse(right_reader);
        const fmp4::InitSegment left = fmp4::InitSegment::parse(left_reader, left_tree);
        const fmp4::InitSegment right = fmp4::InitSegment::parse(right_reader, right_tree);
        const std::vector<fmp4::InitMismatch> diffs = fmp4::compareInitSegments(left, right);
        passed = diffs.empty();
        std::ostringstream out;
        out << "{\"init_compatible\":" << (passed ? "true" : "false")
            << ",\"init_mismatches\":" << mismatchJson(diffs) << ",\"passed\":"
            << (passed ? "true" : "false") << ",\"status\":\"" << (passed ? "ok" : "failed")
            << "\"}";
        if (!json_path.empty()) writeTextFile(json_path, out.str());
        std::cout << out.str() << '\n';
    }
    return passed ? 0 : 1;
}

void printHelp() {
    std::cout << "usage: fmp4tool <probe|validate|repack|compare> ...\n"
                 "  probe FILE [--json PATH]\n"
                 "  validate FILE [--json PATH] [--init INIT_FILE]\n"
                 "  repack INPUT -o OUTPUT [--tfdt-version 1] [--renumber]\n"
                 "  compare LEFT RIGHT [--json PATH]\n";
}

}  // namespace

int main(int argc, char** argv) {
    try {
        if (argc < 2) {
            printHelp();
            return static_cast<int>(ExitCode::UsageOrIo);
        }
        const std::string command = argv[1];
        if (command == "probe") return runProbe(argc, argv);
        if (command == "validate") return runValidate(argc, argv);
        if (command == "repack") return runRepack(argc, argv);
        if (command == "compare") return runCompare(argc, argv);
        if (command == "--help" || command == "-h" || command == "help") {
            printHelp();
            return 0;
        }
        throw Error(ExitCode::UsageOrIo, "unknown command: " + command);
    } catch (const Error& error) {
        std::cerr << "fmp4tool: " << error.what() << '\n';
        return static_cast<int>(error.code());
    } catch (const std::exception& error) {
        std::cerr << "fmp4tool: " << error.what() << '\n';
        return static_cast<int>(ExitCode::UsageOrIo);
    }
}

