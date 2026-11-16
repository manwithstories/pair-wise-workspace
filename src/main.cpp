#include "gop_builder.h"
#include "report.h"

#include <algorithm>
#include <filesystem>
#include <iostream>
#include <string>
#include <vector>

namespace {

constexpr int kExitOk = 0;
constexpr int kExitFailure = 1;
constexpr int kExitVerify = 2;
constexpr int kExitUsage = 3;

void printUsage(std::ostream& out) {
    out << "usage:\n"
        << "  h264gop probe <file.h264>\n"
        << "  h264gop index <directory-or-file> [--json <output.json>]\n"
        << "  h264gop verify <file.h264> [--expect-progressive]\n"
        << "exit codes: 0 ok, 1 read/parse failure, 2 progressive check failed, 3 usage\n";
}

int usageError(const std::string& message) {
    std::cerr << "error: " << message << '\n';
    printUsage(std::cerr);
    return kExitUsage;
}

bool hasAnnexBExtension(const std::filesystem::path& path) {
    const std::string extension = path.extension().string();
    return extension == ".h264" || extension == ".264";
}

std::vector<std::string> collectInputs(const std::string& input) {
    std::error_code error;
    if (!std::filesystem::exists(input, error)) {
        throw std::runtime_error("input does not exist: " + input);
    }
    if (std::filesystem::is_regular_file(input, error)) {
        return {input};
    }
    if (!std::filesystem::is_directory(input, error)) {
        throw std::runtime_error("input is not a file or directory: " + input);
    }

    std::vector<std::string> files;
    for (std::filesystem::recursive_directory_iterator it(input, error), end;
         it != end && !error; it.increment(error)) {
        if (it->is_regular_file(error) && hasAnnexBExtension(it->path())) {
            files.push_back(it->path().string());
        }
    }
    if (error) {
        throw std::runtime_error("directory scan failed: " + error.message());
    }
    std::sort(files.begin(), files.end());
    return files;
}

int runProbe(int argc, char** argv) {
    if (argc != 3 || std::string(argv[2]).rfind("-", 0) == 0) {
        return usageError("probe expects exactly one input file");
    }
    const h264::FileAnalysis analysis = h264::analyzeFile(argv[2]);
    h264::printProbe(std::cout, analysis);
    return kExitOk;
}

int runVerify(int argc, char** argv) {
    std::string file;
    bool expectProgressive = false;
    for (int i = 2; i < argc; ++i) {
        const std::string argument = argv[i];
        if (argument == "--expect-progressive") {
            expectProgressive = true;
        } else if (!argument.empty() && argument[0] == '-') {
            return usageError("unknown verify option: " + argument);
        } else if (file.empty()) {
            file = argument;
        } else {
            return usageError("verify accepts only one input file");
        }
    }
    if (file.empty()) {
        return usageError("verify requires an input file");
    }
    const h264::FileAnalysis analysis = h264::analyzeFile(file);
    if (expectProgressive) {
        const auto issue = h264::firstProgressivenessIssue(analysis);
        if (issue) {
            std::cout << "verify: FAIL offset=" << issue->offset
                      << " message=" << issue->message << '\n';
            return kExitVerify;
        }
        std::cout << "verify: OK progressive\n";
        return kExitOk;
    }
    std::cout << "verify: OK sequences=" << analysis.sequences.size()
              << " gops=" << h264::totalGops(analysis)
              << " frames=" << h264::totalFrames(analysis) << '\n';
    return kExitOk;
}

int runIndex(int argc, char** argv) {
    std::string input;
    std::string jsonPath;
    for (int i = 2; i < argc; ++i) {
        const std::string argument = argv[i];
        if (argument == "--json") {
            if (!jsonPath.empty() || i + 1 >= argc) {
                return usageError("--json requires exactly one output path");
            }
            jsonPath = argv[++i];
        } else if (!argument.empty() && argument[0] == '-') {
            return usageError("unknown index option: " + argument);
        } else if (input.empty()) {
            input = argument;
        } else {
            return usageError("index accepts only one input path");
        }
    }
    if (input.empty()) {
        return usageError("index requires an input directory or file");
    }

    const std::vector<std::string> inputs = collectInputs(input);
    if (inputs.empty()) {
        throw std::runtime_error("no H.264 files found under: " + input);
    }
    std::vector<h264::FileAnalysis> analyses;
    analyses.reserve(inputs.size());
    for (const std::string& path : inputs) {
        analyses.push_back(h264::analyzeFile(path));
    }
    if (!jsonPath.empty()) {
        const std::filesystem::path output(jsonPath);
        if (output.has_parent_path()) {
            std::error_code error;
            std::filesystem::create_directories(output.parent_path(), error);
            if (error) {
                throw std::runtime_error("cannot create JSON directory: " + error.message());
            }
        }
        std::string error;
        if (!h264::writeIndexJson(jsonPath, analyses, error)) {
            throw std::runtime_error(error);
        }
    }
    h264::printIndexSummary(std::cout, analyses, jsonPath);
    return kExitOk;
}

int run(int argc, char** argv) {
    if (argc < 2) {
        return usageError("missing subcommand");
    }
    const std::string command = argv[1];
    if (command == "-h" || command == "--help" || command == "help") {
        printUsage(std::cout);
        return kExitOk;
    }
    if (command == "probe") {
        return runProbe(argc, argv);
    }
    if (command == "verify") {
        return runVerify(argc, argv);
    }
    if (command == "index") {
        return runIndex(argc, argv);
    }
    return usageError("unknown subcommand: " + command);
}

}  // namespace

int main(int argc, char** argv) {
    try {
        return run(argc, argv);
    } catch (const h264::ScanError& error) {
        std::cerr << "error: " << error.what() << '\n';
        return kExitFailure;
    } catch (const std::exception& error) {
        std::cerr << "error: " << error.what() << '\n';
        return kExitFailure;
    }
}
