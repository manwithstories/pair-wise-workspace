#include "error.hpp"
#include "framebuffer.hpp"
#include "hash.hpp"
#include "json.hpp"
#include "timeline.hpp"
#include "time_map.hpp"
#include "transition_kernel.hpp"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <map>
#include <mutex>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

namespace bake {
namespace {

using Path = std::filesystem::path;

struct Arguments {
    std::string command;
    std::map<std::string, std::string> values;
    std::set<std::string> flags;

    bool has(const std::string& key) const { return values.count(key) != 0; }
    bool flag(const std::string& key) const { return flags.count(key) != 0; }
    std::string value(const std::string& key) const {
        const auto it = values.find(key);
        return it == values.end() ? std::string() : it->second;
    }
};

struct Runtime {
    std::string command = "transition_bake";
    std::string report_path = "report.json";
};

Runtime runtime_state;
constexpr std::size_t kMaxJobs = 64;

[[noreturn]] void argumentError(const std::string& message) {
    throw BakeError(ExitCode::bad_arguments, message);
}

int64_t parseInteger(const std::string& text, const std::string& option) {
    if (text.empty()) argumentError(option + " requires an integer");
    std::size_t position = 0;
    bool negative = false;
    if (text[position] == '-') {
        negative = true;
        ++position;
    }
    if (position == text.size()) argumentError(option + " requires an integer");
    int64_t value = 0;
    for (; position < text.size(); ++position) {
        const char c = text[position];
        if (c < '0' || c > '9') argumentError(option + " must be an integer");
        if (value > (INT64_MAX - (c - '0')) / 10) {
            argumentError(option + " overflows int64");
        }
        value = value * 10 + (c - '0');
    }
    return negative ? -value : value;
}

std::size_t parseJobs(const Arguments& args) {
    if (!args.has("jobs")) return 1;
    const int64_t jobs = parseInteger(args.value("jobs"), "--jobs");
    if (jobs <= 0 || jobs > static_cast<int64_t>(kMaxJobs)) {
        argumentError("--jobs must be between 1 and 64");
    }
    return static_cast<std::size_t>(jobs);
}

Arguments parseArguments(int argc, char** argv) {
    if (argc < 2) argumentError("missing command (render, verify, report, selftest)");
    Arguments args;
    args.command = argv[1];
    if (args.command != "render" && args.command != "verify" &&
        args.command != "report" && args.command != "selftest") {
        argumentError("unknown command: " + args.command);
    }
    const std::set<std::string> value_options = {
        "timeline", "src", "out", "seed", "report", "dir", "expect", "jobs"};
    for (int i = 2; i < argc; ++i) {
        std::string option = argv[i];
        if (option.rfind("--", 0) != 0) {
            argumentError("unexpected argument: " + option);
        }
        option.erase(0, 2);
        if (option == "strict" || option == "continue-on-error") {
            args.flags.insert(option);
            continue;
        }
        if (value_options.count(option) == 0) {
            argumentError("unknown option: --" + option);
        }
        if (i + 1 >= argc) argumentError("--" + option + " requires a value");
        if (args.values.count(option) != 0) {
            argumentError("duplicate option: --" + option);
        }
        args.values[option] = argv[++i];
    }
    return args;
}

std::string requiredValue(const Arguments& args, const std::string& option) {
    if (!args.has(option)) argumentError("missing required option --" + option);
    return args.value(option);
}

std::string readText(const Path& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        throw BakeError(ExitCode::invalid_input, "cannot open file: " + path.string());
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (!input.good() && !input.eof()) {
        throw BakeError(ExitCode::invalid_input, "cannot read file: " + path.string());
    }
    return buffer.str();
}

void writeJsonFile(const Path& path, const Json& value) {
    if (path.empty()) {
        throw BakeError(ExitCode::write_failed, "empty report path");
    }
    std::error_code error;
    if (!path.parent_path().empty()) {
        std::filesystem::create_directories(path.parent_path(), error);
        if (error) {
            throw BakeError(ExitCode::write_failed,
                            "cannot create report directory: " + error.message());
        }
    }
    const Path temporary = path.string() + ".tmp";
    {
        std::ofstream output(temporary, std::ios::binary | std::ios::trunc);
        if (!output) {
            throw BakeError(ExitCode::write_failed,
                            "cannot create report: " + path.string());
        }
        output << dumpJson(value) << '\n';
        output.flush();
        if (!output) {
            throw BakeError(ExitCode::write_failed,
                            "failed writing report: " + path.string());
        }
    }
    std::filesystem::rename(temporary, path, error);
    if (error) {
        std::filesystem::remove(temporary, error);
        throw BakeError(ExitCode::write_failed,
                        "cannot replace report: " + path.string());
    }
}

void emitJson(const Json& value, const Path& report_path) {
    if (!report_path.empty()) writeJsonFile(report_path, value);
    std::cout << dumpJson(value) << '\n';
    if (!std::cout) {
        throw BakeError(ExitCode::write_failed, "cannot write stdout");
    }
}

Json errorDocument(const std::string& command, int code,
                   const std::string& message) {
    Json document = Json::object();
    document.set("command", Json::string(command));
    document.set("ok", Json::boolean(false));
    document.set("total_frames", Json::integer(0));
    document.set("reproducible", Json::boolean(false));
    document.set("speed_segments", Json::integer(0));
    document.set("duplicate_frames", Json::integer(0));
    document.set("skipped_frames", Json::integer(0));
    document.set("exit_code", Json::integer(code));
    Json errors = Json::array();
    errors.push(Json::string(message));
    document.set("errors", std::move(errors));
    document.set("classes", Json::array());
    return document;
}

template <typename Container>
Json integerArray(const Container& values) {
    Json array = Json::array();
    for (const auto& value : values) {
        array.push(Json::integer(static_cast<int64_t>(value)));
    }
    return array;
}

Json evidenceJson(const ClassEvidence& evidence) {
    Json object = Json::object();
    object.set("class", Json::string(evidence.class_name));
    object.set("frame_count", Json::integer(evidence.frame_count));
    object.set("first_frame_hash", Json::string(hashHex(evidence.first_frame_hash)));
    object.set("last_frame_hash", Json::string(hashHex(evidence.last_frame_hash)));
    object.set("mean_abs_diff", Json::integer(evidence.mean_abs_diff));
    object.set("max_abs_diff", Json::integer(evidence.max_abs_diff));
    object.set("changed_pixels", Json::integer(evidence.changed_pixels));
    object.set("diff_histogram", integerArray(evidence.diff_histogram));
    object.set("diff_peak_frame", Json::integer(evidence.diff_peak_frame));
    object.set("diff_curve", integerArray(evidence.diff_curve));
    object.set("curve_kind", Json::string(evidence.curve_kind));
    return object;
}

Json classesJson(int width, int height, uint64_t seed) {
    Json classes = Json::array();
    for (const ClassEvidence& evidence : computeClassEvidence(width, height, seed)) {
        classes.push(evidenceJson(evidence));
    }
    return classes;
}

Json errorsJson(const std::vector<std::string>& errors) {
    Json array = Json::array();
    for (const std::string& error : errors) array.push(Json::string(error));
    return array;
}

void addError(std::vector<std::string>& errors, ExitCode& code,
              const std::string& message, ExitCode candidate) {
    errors.push_back(message);
    if (code == ExitCode::ok) code = candidate;
}

void appendSourceErrors(const std::vector<LoadedClip>& sources,
                        std::vector<std::string>& errors, ExitCode& code) {
    std::set<std::string> known(errors.begin(), errors.end());
    for (const LoadedClip& clip : sources) {
        for (const std::string& error : clip.frame_errors) {
            if (!error.empty() && known.insert(error).second) {
                addError(errors, code, error, ExitCode::invalid_input);
            }
        }
    }
}

Path resolveRelative(const Path& base, const std::string& value) {
    const Path path(value);
    return path.is_absolute() ? path : base / path;
}

struct RenderState {
    std::vector<uint64_t> hashes;
    std::vector<uint8_t> rendered;
    std::vector<std::string> frame_errors;
    std::vector<ExitCode> frame_codes;
};

void removeOldFrames(const Path& output_directory) {
    std::error_code error;
    if (!std::filesystem::exists(output_directory, error)) {
        std::filesystem::create_directories(output_directory, error);
        if (error) {
            throw BakeError(ExitCode::write_failed,
                            "cannot create output directory: " + error.message());
        }
        return;
    }
    if (!std::filesystem::is_directory(output_directory, error) || error) {
        throw BakeError(ExitCode::write_failed,
                        "output path is not a directory: " + output_directory.string());
    }
    for (const auto& entry : std::filesystem::directory_iterator(output_directory, error)) {
        if (error) {
            throw BakeError(ExitCode::write_failed,
                            "cannot enumerate output directory: " + error.message());
        }
        if (entry.is_regular_file(error) && entry.path().extension() == ".ppm") {
            std::filesystem::remove(entry.path(), error);
            if (error) {
                throw BakeError(ExitCode::write_failed,
                                "cannot remove stale frame: " + entry.path().string());
            }
        }
    }
}

int renderCommand(const Arguments& args) {
    const Path timeline_path(requiredValue(args, "timeline"));
    const Path source_root(requiredValue(args, "src"));
    const Path output_directory(requiredValue(args, "out"));
    const bool continue_on_error = args.flag("continue-on-error");
    const std::size_t jobs = parseJobs(args);
    Timeline timeline = Timeline::load(timeline_path.string());
    if (args.has("seed")) {
        const int64_t seed = parseInteger(args.value("seed"), "--seed");
        if (seed < 0) argumentError("--seed must be non-negative");
        timeline.seed = static_cast<uint64_t>(seed);
    }
    const std::vector<LoadedClip> sources =
        timeline.loadSources(source_root.string(), continue_on_error);
    removeOldFrames(output_directory);

    const int64_t total = timeline.totalFrames();
    if (total <= 0 || total > 10000000) {
        throw BakeError(ExitCode::invalid_input, "timeline frame count is unreasonable");
    }
    RenderState state;
    state.hashes.assign(static_cast<std::size_t>(total), 0);
    state.rendered.assign(static_cast<std::size_t>(total), 0);
    state.frame_errors.resize(static_cast<std::size_t>(total));
    state.frame_codes.assign(static_cast<std::size_t>(total), ExitCode::ok);
    std::atomic<bool> stop{false};
    std::atomic<std::size_t> next_frame{0};
    const std::size_t worker_count = std::min<std::size_t>(
        jobs, static_cast<std::size_t>(total));
    auto worker = [&]() {
        FrameBuffer destination(timeline.width, timeline.height);
        for (;;) {
            if (stop.load()) break;
            const std::size_t index = next_frame.fetch_add(1);
            if (index >= static_cast<std::size_t>(total)) break;
            const int64_t frame_number = static_cast<int64_t>(index);
            try {
                timeline.renderFrame(frame_number, sources, destination);
                const uint64_t hash = fnv1a64(destination);
                const Path frame_path = output_directory / frameFileName(frame_number);
                writePpm(frame_path.string(), destination, frame_number);
                state.hashes[index] = hash;
                state.rendered[index] = 1;
            } catch (const BakeError& error) {
                state.frame_errors[index] = error.what();
                state.frame_codes[index] = error.code;
                if (!continue_on_error) stop.store(true);
            } catch (const std::exception& error) {
                state.frame_errors[index] = error.what();
                state.frame_codes[index] = ExitCode::invariant;
                if (!continue_on_error) stop.store(true);
            }
        }
    };
    std::vector<std::thread> threads;
    for (std::size_t i = 1; i < worker_count; ++i) threads.emplace_back(worker);
    worker();
    for (std::thread& thread : threads) thread.join();

    std::vector<std::string> errors;
    ExitCode code = ExitCode::ok;
    for (std::size_t i = 0; i < state.frame_errors.size(); ++i) {
        if (!state.frame_errors[i].empty()) {
            addError(errors, code, state.frame_errors[i], state.frame_codes[i]);
        }
    }
    appendSourceErrors(sources, errors, code);
    int64_t rendered_count = 0;
    for (uint8_t value : state.rendered) rendered_count += value;
    const int64_t skipped = total - rendered_count;

    bool reproducible = true;
    FrameBuffer check_buffer(timeline.width, timeline.height);
    for (int64_t frame = 0; frame < total; ++frame) {
        if (!state.rendered[static_cast<std::size_t>(frame)]) continue;
        timeline.renderFrame(frame, sources, check_buffer);
        if (fnv1a64(check_buffer) != state.hashes[static_cast<std::size_t>(frame)]) {
            reproducible = false;
            addError(errors, code,
                     "non-deterministic frame hash at frame " + std::to_string(frame),
                     ExitCode::invariant);
            break;
        }
    }

    int64_t duplicate_frames = 0;
    int64_t intentional_holds = 0;
    for (int64_t frame = 1; frame < total; ++frame) {
        const std::size_t index = static_cast<std::size_t>(frame);
        if (!state.rendered[index] || !state.rendered[index - 1] ||
            state.hashes[index] != state.hashes[index - 1]) {
            continue;
        }
        if (timeline.isIntentionalHold(frame)) {
            ++intentional_holds;
            if (state.hashes[index] != state.hashes[index - 1]) {
                addError(errors, code,
                         "intentional hold frame hash changed at frame " +
                             std::to_string(frame),
                         ExitCode::invariant);
            }
        } else {
            ++duplicate_frames;
        }
    }
    if (duplicate_frames != 0) {
        addError(errors, code,
                 "unexpected duplicate output frames: " + std::to_string(duplicate_frames),
                 ExitCode::mismatch);
    }
    if (!reproducible) code = ExitCode::invariant;

    Json report = Json::object();
    report.set("command", Json::string("render"));
    report.set("ok", Json::boolean(errors.empty() && skipped == 0 &&
                                   duplicate_frames == 0 && reproducible));
    report.set("width", Json::integer(timeline.width));
    report.set("height", Json::integer(timeline.height));
    report.set("total_frames", Json::integer(total));
    report.set("rendered_frames", Json::integer(rendered_count));
    report.set("classes", classesJson(timeline.width, timeline.height, timeline.seed));
    report.set("reproducible", Json::boolean(reproducible));
    report.set("speed_segments", Json::integer(
        static_cast<int64_t>(timeline.speed.size())));
    report.set("duplicate_frames", Json::integer(duplicate_frames));
    report.set("intentional_holds", Json::integer(intentional_holds));
    report.set("skipped_frames", Json::integer(skipped));
    report.set("errors", errorsJson(errors));
    if (rendered_count > 0) {
        report.set("first_frame_hash", Json::string(hashHex(state.hashes.front())));
        report.set("last_frame_hash", Json::string(hashHex(state.hashes.back())));
    }
    emitJson(report, runtime_state.report_path);
    return static_cast<int>(code);
}

const Json& expectField(const Json& object, const char* key) {
    const Json* value = object.find(key);
    if (value == nullptr) {
        throw BakeError(ExitCode::invalid_input,
                        std::string("expectation is missing field: ") + key);
    }
    return *value;
}

int64_t expectInteger(const Json& object, const char* key) {
    const Json& value = expectField(object, key);
    if (!value.isInteger()) {
        throw BakeError(ExitCode::invalid_input,
                        std::string("expectation field is not an integer: ") + key);
    }
    return value.int_value;
}

std::string expectString(const Json& object, const char* key) {
    const Json& value = expectField(object, key);
    if (!value.isString()) {
        throw BakeError(ExitCode::invalid_input,
                        std::string("expectation field is not a string: ") + key);
    }
    return value.string_value;
}

struct Expectations {
    Path timeline_path;
    Path source_path;
    int width = 0;
    int height = 0;
    int64_t total_frames = 0;
    int64_t class_frame_count = 0;
    int64_t speed_segments = 0;
    std::vector<std::string> classes;
};

Expectations loadExpectations(const Path& path) {
    Json root;
    try {
        root = parseJson(readText(path));
    } catch (const BakeError&) {
        throw;
    } catch (const std::exception& error) {
        throw BakeError(ExitCode::invalid_input,
                        std::string("invalid expectation JSON: ") + error.what());
    }
    if (!root.isObject()) {
        throw BakeError(ExitCode::invalid_input, "expectation root must be an object");
    }
    Expectations result;
    const Path base = path.parent_path();
    result.timeline_path = resolveRelative(base, expectString(root, "timeline"));
    result.source_path = resolveRelative(base, expectString(root, "src"));
    const int64_t width = expectInteger(root, "width");
    const int64_t height = expectInteger(root, "height");
    if (width <= 0 || height <= 0) {
        throw BakeError(ExitCode::invalid_input, "expectation dimensions are invalid");
    }
    result.width = static_cast<int>(width);
    result.height = static_cast<int>(height);
    result.total_frames = expectInteger(root, "total_frames");
    result.class_frame_count = expectInteger(root, "class_frame_count");
    result.speed_segments = expectInteger(root, "speed_segments");
    const Json& classes = expectField(root, "classes");
    if (!classes.isArray()) {
        throw BakeError(ExitCode::invalid_input,
                        "expectation classes must be an array");
    }
    for (const Json& item : classes.array_value) {
        if (!item.isString()) {
            throw BakeError(ExitCode::invalid_input,
                            "expectation class name must be a string");
        }
        result.classes.push_back(item.string_value);
    }
    return result;
}

struct VerifyState {
    std::vector<uint64_t> hashes;
    std::vector<uint8_t> read;
    std::vector<std::string> errors;
    ExitCode code = ExitCode::ok;
    int64_t skipped = 0;
    int64_t duplicate_frames = 0;
    int64_t intentional_holds = 0;
};

void verifyFrames(const Path& directory, const Timeline& timeline,
                  const std::vector<LoadedClip>& sources, bool continue_on_error,
                  VerifyState& state) {
    const int64_t total = timeline.totalFrames();
    state.hashes.assign(static_cast<std::size_t>(total), 0);
    state.read.assign(static_cast<std::size_t>(total), 0);
    for (int64_t frame = 0; frame < total; ++frame) {
        if (!std::filesystem::exists(directory / frameFileName(frame))) {
            ++state.skipped;
        }
    }
    FrameBuffer expected(timeline.width, timeline.height);
    for (int64_t frame = 0; frame < total; ++frame) {
        const std::size_t index = static_cast<std::size_t>(frame);
        try {
            const Path frame_path = directory / frameFileName(frame);
            if (!std::filesystem::exists(frame_path)) {
                addError(state.errors, state.code,
                         "missing output frame " + std::to_string(frame),
                         ExitCode::mismatch);
                if (!continue_on_error) break;
                continue;
            }
            const FrameBuffer actual = readPpm(frame_path.string(), frame);
            if (actual.width != timeline.width || actual.height != timeline.height) {
                addError(state.errors, state.code,
                         "output frame dimensions differ at frame " +
                             std::to_string(frame),
                         ExitCode::mismatch);
                if (!continue_on_error) break;
                continue;
            }
            timeline.renderFrame(frame, sources, expected);
            const uint64_t expected_hash = fnv1a64(expected);
            const uint64_t actual_hash = fnv1a64(actual);
            state.hashes[index] = actual_hash;
            state.read[index] = 1;
            if (expected_hash != actual_hash) {
                addError(state.errors, state.code,
                         "frame hash mismatch at frame " + std::to_string(frame) +
                             " expected=" + hashHex(expected_hash) +
                             " actual=" + hashHex(actual_hash),
                         ExitCode::mismatch);
                if (!continue_on_error) break;
            }
        } catch (const BakeError& error) {
            addError(state.errors, state.code, error.what(),
                     error.code == ExitCode::write_failed ? ExitCode::invalid_input
                                                          : error.code);
            if (!continue_on_error) break;
        } catch (const std::exception& error) {
            addError(state.errors, state.code, error.what(), ExitCode::invalid_input);
            if (!continue_on_error) break;
        }
    }
    for (int64_t frame = 1; frame < total; ++frame) {
        const std::size_t index = static_cast<std::size_t>(frame);
        if (!state.read[index] || !state.read[index - 1] ||
            state.hashes[index] != state.hashes[index - 1]) {
            continue;
        }
        if (timeline.isIntentionalHold(frame)) {
            ++state.intentional_holds;
        } else {
            ++state.duplicate_frames;
        }
    }
}

void checkExpectationMetadata(const Expectations& expected, const Timeline& timeline,
                              const std::vector<ClassEvidence>& classes,
                              std::vector<std::string>& errors, ExitCode& code) {
    if (expected.width != timeline.width || expected.height != timeline.height) {
        addError(errors, code, "expectation dimensions do not match timeline",
                 ExitCode::mismatch);
    }
    if (expected.total_frames != timeline.totalFrames()) {
        addError(errors, code, "expectation total_frames does not match timeline",
                 ExitCode::mismatch);
    }
    if (expected.speed_segments != static_cast<int64_t>(timeline.speed.size())) {
        addError(errors, code, "expectation speed_segments does not match timeline",
                 ExitCode::mismatch);
    }
    if (expected.classes.size() != classes.size()) {
        addError(errors, code, "expectation class count does not match registry",
                 ExitCode::mismatch);
        return;
    }
    std::set<std::string> expected_names(expected.classes.begin(), expected.classes.end());
    for (const ClassEvidence& evidence : classes) {
        if (expected_names.count(evidence.class_name) == 0) {
            addError(errors, code, "expectation is missing class: " + evidence.class_name,
                     ExitCode::mismatch);
        }
        if (evidence.frame_count != expected.class_frame_count) {
            addError(errors, code, "class frame_count does not match expectation",
                     ExitCode::mismatch);
        }
    }
}

void checkStrictDirectory(const Path& directory, const Timeline& timeline,
                          std::vector<std::string>& errors, ExitCode& code) {
    std::set<std::string> expected_names;
    for (int64_t frame = 0; frame < timeline.totalFrames(); ++frame) {
        expected_names.insert(frameFileName(frame));
    }
    std::error_code error;
    for (const auto& entry : std::filesystem::directory_iterator(directory, error)) {
        if (error) {
            addError(errors, code, "cannot enumerate output directory: " + error.message(),
                     ExitCode::invalid_input);
            return;
        }
        if (!entry.is_regular_file(error) || error) {
            error.clear();
            continue;
        }
        if (entry.path().extension() != ".ppm") continue;
        if (expected_names.count(entry.path().filename().string()) == 0) {
            addError(errors, code,
                     "unexpected PPM in output: " + entry.path().filename().string(),
                     ExitCode::mismatch);
        }
    }
}

Json buildVerificationDocument(const std::string& command, const Timeline& timeline,
                               const VerifyState& state, int64_t observed_frames) {
    std::vector<std::string> errors = state.errors;
    ExitCode code = state.code;
    if (state.duplicate_frames != 0) {
        addError(errors, code,
                 "unexpected duplicate output frames: " +
                     std::to_string(state.duplicate_frames),
                 ExitCode::mismatch);
    }
    if (state.skipped != 0) {
        addError(errors, code,
                 "skipped output frames: " + std::to_string(state.skipped),
                 ExitCode::mismatch);
    }
    Json report = Json::object();
    report.set("command", Json::string(command));
    report.set("ok", Json::boolean(errors.empty()));
    report.set("width", Json::integer(timeline.width));
    report.set("height", Json::integer(timeline.height));
    report.set("total_frames", Json::integer(timeline.totalFrames()));
    report.set("observed_frames", Json::integer(observed_frames));
    report.set("classes", classesJson(timeline.width, timeline.height, timeline.seed));
    report.set("reproducible", Json::boolean(errors.empty()));
    report.set("speed_segments", Json::integer(
        static_cast<int64_t>(timeline.speed.size())));
    report.set("duplicate_frames", Json::integer(state.duplicate_frames));
    report.set("intentional_holds", Json::integer(state.intentional_holds));
    report.set("skipped_frames", Json::integer(state.skipped));
    report.set("errors", errorsJson(errors));
    if (observed_frames > 0) {
        for (std::size_t i = 0; i < state.read.size(); ++i) {
            if (state.read[i]) {
                report.set("first_frame_hash", Json::string(hashHex(state.hashes[i])));
                break;
            }
        }
        for (std::size_t i = state.read.size(); i > 0; --i) {
            if (state.read[i - 1]) {
                report.set("last_frame_hash", Json::string(hashHex(state.hashes[i - 1])));
                break;
            }
        }
    }
    return report;
}

int verifyCommand(const Arguments& args) {
    const Path output_directory(requiredValue(args, "dir"));
    const Path expect_path(requiredValue(args, "expect"));
    const bool strict = args.flag("strict");
    const bool continue_on_error = args.flag("continue-on-error");
    const Expectations expected = loadExpectations(expect_path);
    Timeline timeline = Timeline::load(expected.timeline_path.string());
    const std::vector<LoadedClip> sources =
        timeline.loadSources(expected.source_path.string(), continue_on_error);
    std::error_code directory_error;
    if (!std::filesystem::is_directory(output_directory, directory_error) ||
        directory_error) {
        throw BakeError(ExitCode::invalid_input,
                        "output directory does not exist: " + output_directory.string());
    }
    VerifyState state;
    checkExpectationMetadata(expected, timeline,
                             computeClassEvidence(timeline.width, timeline.height,
                                                  timeline.seed),
                             state.errors, state.code);
    verifyFrames(output_directory, timeline, sources, continue_on_error, state);
    appendSourceErrors(sources, state.errors, state.code);
    if (strict) {
        checkStrictDirectory(output_directory, timeline, state.errors, state.code);
    }
    int64_t observed_frames = 0;
    for (uint8_t value : state.read) observed_frames += value;
    Json report = buildVerificationDocument("verify", timeline, state, observed_frames);
    const bool ok = report.at("ok").bool_value;
    emitJson(report, runtime_state.report_path);
    if (ok) return 0;
    return static_cast<int>(state.code == ExitCode::ok ? ExitCode::mismatch : state.code);
}

int reportCommand(const Arguments& args) {
    const Path output_directory(requiredValue(args, "dir"));
    Timeline timeline = Timeline::load(requiredValue(args, "timeline"));
    const std::vector<LoadedClip> sources =
        timeline.loadSources(requiredValue(args, "src"),
                             args.flag("continue-on-error"));
    VerifyState state;
    verifyFrames(output_directory, timeline, sources,
                 args.flag("continue-on-error"), state);
    appendSourceErrors(sources, state.errors, state.code);
    if (args.flag("strict")) {
        checkStrictDirectory(output_directory, timeline, state.errors, state.code);
    }
    int64_t observed_frames = 0;
    for (uint8_t value : state.read) observed_frames += value;
    Json report = buildVerificationDocument("report", timeline, state, observed_frames);
    const bool ok = report.at("ok").bool_value;
    emitJson(report, runtime_state.report_path);
    if (ok) return 0;
    return static_cast<int>(state.code == ExitCode::ok ? ExitCode::mismatch : state.code);
}

void selfCheck(bool condition, const std::string& message, int& checks) {
    if (!condition) {
        throw BakeError(ExitCode::invariant, "selftest failed: " + message);
    }
    ++checks;
}

template <typename Function>
void checkThrowsInput(Function function, const std::string& message, int& checks) {
    try {
        function();
    } catch (const BakeError& error) {
        if (error.code == ExitCode::invalid_input) {
            ++checks;
            return;
        }
        throw BakeError(ExitCode::invariant,
                        "selftest wrong exit code for: " + message);
    }
    throw BakeError(ExitCode::invariant,
                    "selftest expected an input error: " + message);
}

int selftestCommand(const Arguments&) {
    int checks = 0;
    FrameBuffer front(12, 9);
    FrameBuffer back(12, 9);
    for (int y = 0; y < front.height; ++y) {
        for (int x = 0; x < front.width; ++x) {
            front.setPixel(x, y, static_cast<uint8_t>(x * 17 + y),
                           static_cast<uint8_t>(x + y * 23),
                           static_cast<uint8_t>(x * 3 + y * 5));
            back.setPixel(x, y, static_cast<uint8_t>(255 - x * 7 - y),
                          static_cast<uint8_t>(x * 13 + y * 29),
                          static_cast<uint8_t>((x * y + 91) & 0xff));
        }
    }
    const uint64_t front_hash = fnv1a64(front);
    const uint64_t back_hash = fnv1a64(back);
    selfCheck(kernelRegistry().size() == 5U, "five kernels registered", checks);
    FrameBuffer destination(12, 9);
    for (const KernelEntry& entry : kernelRegistry()) {
        applyKernel(entry.name, front, back, destination, 0,
                    KernelParams{7, 0});
        selfCheck(fnv1a64(destination) == front_hash,
                  entry.name + " progress 0 identity", checks);
        applyKernel(entry.name, front, back, destination, 65536,
                    KernelParams{7, 1});
        selfCheck(fnv1a64(destination) == back_hash,
                  entry.name + " progress 65536 identity", checks);
        applyKernel(entry.name, front, back, destination, 32768,
                    KernelParams{7, 2});
        selfCheck(destination.fullyInitialized(),
                  entry.name + " initializes every pixel", checks);
    }
    checkThrowsInput([&]() {
        applyKernel("linear_blend", front, back, destination, 65537,
                    KernelParams{7, 3});
    }, "progress bounds", checks);
    FrameBuffer untouched(4, 4);
    selfCheck(!untouched.fullyInitialized(), "new frame starts uninitialized", checks);

    const Path temporary = std::filesystem::temp_directory_path() /
        ("pairwise_bake_selftest_" + std::to_string(
            std::chrono::steady_clock::now().time_since_epoch().count()));
    std::error_code filesystem_error;
    std::filesystem::create_directories(temporary, filesystem_error);
    try {
        const Path good = temporary / "good.ppm";
        writePpm(good.string(), front, 0);
        const FrameBuffer restored = readPpm(good.string(), 0);
        selfCheck(fnv1a64(restored) == front_hash, "P6 round trip", checks);
        const Path bad = temporary / "bad.ppm";
        {
            std::ofstream output(bad, std::ios::binary | std::ios::trunc);
            output << "P6\n1 1\n254\n";
            const char pixel[3] = {1, 2, 3};
            output.write(pixel, 3);
        }
        checkThrowsInput([&]() { (void)readPpm(bad.string(), 0); },
                         "PPM maxval rejection", checks);
    } catch (...) {
        std::filesystem::remove_all(temporary, filesystem_error);
        throw;
    }
    std::filesystem::remove_all(temporary, filesystem_error);

    const TimeMap hold({{5, 4, 0, 1}}, 10, false);
    for (int64_t i = 0; i < hold.totalFrames(); ++i) {
        const TimePoint point = hold.map(i);
        selfCheck(point.source_frame == 4 && point.progress_q16 == 0,
                  "zero-rate hold", checks);
    }
    const TimeMap reverse({{6, 9, -1, 1}}, 10, false);
    for (int64_t i = 1; i < reverse.totalFrames(); ++i) {
        selfCheck(reverse.map(i).source_frame < reverse.map(i - 1).source_frame,
                  "reverse source decreases", checks);
    }
    const TimeMap half({{6, 0, 1, 2}}, 10, false);
    selfCheck(half.map(0).source_frame == 0 && half.map(1).source_frame == 0 &&
                  half.map(2).source_frame == 1,
              "half-rate rational integration", checks);
    selfCheck(half.map(1).progress_q16 == 32768, "Q16.16 half progress", checks);
    checkThrowsInput([]() { (void)TimeMap({{2, 0, 1, 0}}, 10, false); },
                     "zero speed denominator", checks);
    checkThrowsInput([]() { (void)TimeMap({{4, 1, -1, 1}}, 10, false); },
                     "reverse crosses source start", checks);
    const TimeMap continuous({{6, 0, 1, 2}, {6, 3, 2, 1}, {6, 15, 3, 1},
                              {6, 33, 0, 1}, {6, 33, -1, 1}, {6, 27, 1, 1}},
                             35, true);
    selfCheck(continuous.totalFrames() == 36, "six speed intervals", checks);

    Json report = Json::object();
    report.set("command", Json::string("selftest"));
    report.set("ok", Json::boolean(true));
    report.set("checks", Json::integer(checks));
    report.set("width", Json::integer(64));
    report.set("height", Json::integer(48));
    report.set("total_frames", Json::integer(0));
    report.set("classes", classesJson(64, 48, 7));
    report.set("reproducible", Json::boolean(true));
    report.set("speed_segments", Json::integer(
        static_cast<int64_t>(continuous.intervals().size())));
    report.set("duplicate_frames", Json::integer(0));
    report.set("skipped_frames", Json::integer(0));
    report.set("errors", Json::array());
    emitJson(report, runtime_state.report_path);
    return 0;
}

}  // namespace
}  // namespace bake

int main(int argc, char** argv) {
    using namespace bake;
    try {
        const Arguments args = parseArguments(argc, argv);
        runtime_state.command = args.command;
        if (args.has("report")) runtime_state.report_path = args.value("report");
        if (args.command == "render") return renderCommand(args);
        if (args.command == "verify") return verifyCommand(args);
        if (args.command == "report") return reportCommand(args);
        return selftestCommand(args);
    } catch (const BakeError& error) {
        const int code = static_cast<int>(error.code);
        Json document = errorDocument(runtime_state.command, code, error.what());
        try {
            writeJsonFile(runtime_state.report_path, document);
        } catch (...) {
        }
        std::cout << dumpJson(document) << '\n';
        return code;
    } catch (const std::exception& error) {
        const int code = static_cast<int>(ExitCode::invariant);
        Json document = errorDocument(runtime_state.command, code, error.what());
        try {
            writeJsonFile(runtime_state.report_path, document);
        } catch (...) {
        }
        std::cout << dumpJson(document) << '\n';
        return code;
    }
}
