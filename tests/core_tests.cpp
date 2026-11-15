#include "box_reader.h"
#include "box_tree.h"

#include <filesystem>
#include <fstream>
#include <iostream>
#include <string>
#include <vector>

namespace {

using Bytes = std::vector<uint8_t>;
int failures = 0;

void check(bool condition, const std::string& message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << '\n';
        ++failures;
    }
}

void putU32(Bytes& out, uint32_t value) {
    out.push_back(static_cast<uint8_t>(value >> 24));
    out.push_back(static_cast<uint8_t>(value >> 16));
    out.push_back(static_cast<uint8_t>(value >> 8));
    out.push_back(static_cast<uint8_t>(value));
}

void putU64(Bytes& out, uint64_t value) {
    putU32(out, static_cast<uint32_t>(value >> 32));
    putU32(out, static_cast<uint32_t>(value));
}

void append(Bytes& target, const Bytes& source) {
    target.insert(target.end(), source.begin(), source.end());
}

Bytes makeBox32(const std::string& type, const Bytes& payload) {
    Bytes box;
    putU32(box, static_cast<uint32_t>(payload.size() + 8));
    box.insert(box.end(), type.begin(), type.end());
    append(box, payload);
    return box;
}

Bytes makeLargeBox(const std::string& type, const Bytes& payload) {
    Bytes box;
    putU32(box, 1);
    box.insert(box.end(), type.begin(), type.end());
    putU64(box, payload.size() + 16);
    append(box, payload);
    return box;
}

Bytes makeZeroSizedBox(const std::string& type, const Bytes& payload) {
    Bytes box;
    putU32(box, 0);
    box.insert(box.end(), type.begin(), type.end());
    append(box, payload);
    return box;
}

void writeFile(const std::filesystem::path& path, const Bytes& bytes) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    output.write(reinterpret_cast<const char*>(bytes.data()),
                 static_cast<std::streamsize>(bytes.size()));
}

void testBoxLengths(const std::filesystem::path& dir) {
    Bytes file = makeBox32("free", {1, 2, 3, 4});
    append(file, makeLargeBox("skip", {}));
    append(file, makeZeroSizedBox("mdat", {9, 8, 7, 6}));
    const std::filesystem::path path = dir / "lengths.mp4";
    writeFile(path, file);

    fmp4::BoxReader reader(path.string());
    const fmp4::BoxTree tree = fmp4::BoxTree::parse(reader);
    check(tree.roots().size() == 3, "three top-level length forms parse");
    if (tree.roots().size() == 3) {
        check(tree.roots()[0].type == "free" && tree.roots()[0].size == 12,
              "32-bit box size");
        check(tree.roots()[1].type == "skip" && tree.roots()[1].size == 16 &&
                  tree.roots()[1].header_size == 16,
              "64-bit largesize box");
        check(tree.roots()[2].type == "mdat" && tree.roots()[2].size == 12 &&
                  tree.roots()[2].payloadSize() == 4,
              "size zero extends to end of file");
    }
}

void testTruncation(const std::filesystem::path& dir) {
    Bytes file;
    putU32(file, 20);
    file.insert(file.end(), {'f', 'r', 'e', 'e'});
    file.insert(file.end(), {0, 0, 0, 0});
    const std::filesystem::path path = dir / "truncated.mp4";
    writeFile(path, file);
    try {
        fmp4::BoxReader reader(path.string());
        (void)fmp4::BoxTree::parse(reader);
        check(false, "truncated box must fail");
    } catch (const fmp4::Error& error) {
        check(error.code() == fmp4::ExitCode::ParseFailed, "truncation is a parse failure");
        check(error.offset() == 0, "truncation reports box byte offset");
        check(std::string(error.what()).find("byte offset") != std::string::npos,
              "truncation message contains byte offset");
    }
}

void testDepth(const std::filesystem::path& dir) {
    Bytes allowed = makeBox32("free", {});
    for (int i = 0; i < 32; ++i) allowed = makeBox32("moov", allowed);
    const std::filesystem::path allowed_path = dir / "depth32.mp4";
    writeFile(allowed_path, allowed);
    try {
        fmp4::BoxReader reader(allowed_path.string());
        const fmp4::BoxTree tree = fmp4::BoxTree::parse(reader);
        check(tree.roots().size() == 1, "32 nested levels are accepted");
    } catch (const fmp4::Error& error) {
        check(false, std::string("depth 32 should parse: ") + error.what());
    }

    Bytes rejected = allowed;
    rejected = makeBox32("moov", rejected);
    const std::filesystem::path rejected_path = dir / "depth33.mp4";
    writeFile(rejected_path, rejected);
    try {
        fmp4::BoxReader reader(rejected_path.string());
        (void)fmp4::BoxTree::parse(reader);
        check(false, "more than 32 nested levels must fail");
    } catch (const fmp4::Error& error) {
        check(error.code() == fmp4::ExitCode::ParseFailed, "deep nesting is a parse failure");
        check(std::string(error.what()).find("32") != std::string::npos,
              "deep nesting error mentions the limit");
    }
}

}  // namespace

int main() {
    const std::filesystem::path dir =
        std::filesystem::temp_directory_path() / "fmp4guard-core-tests";
    std::filesystem::remove_all(dir);
    std::filesystem::create_directories(dir);
    testBoxLengths(dir);
    testTruncation(dir);
    testDepth(dir);
    std::filesystem::remove_all(dir);
    if (failures != 0) {
        std::cerr << failures << " core test(s) failed\n";
        return 1;
    }
    std::cout << "core tests passed\n";
    return 0;
}

