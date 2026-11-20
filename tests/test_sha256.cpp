#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <fstream>
#include <string>
#include <unistd.h>

#include "mediacore/sha256.hpp"

using namespace mediacore;

TEST_CASE("SHA-256 matches published NIST vectors", "[hash]") {
    Sha256 h;
    REQUIRE(h.hexDigest() ==
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");

    Sha256 abc;
    abc.update(std::string_view("abc"));
    REQUIRE(abc.hexDigest() ==
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");

    // Two-block message spanning the 64-byte boundary.
    const std::string longText(128, 'x');
    Sha256 one;
    one.update(longText);
    const std::string digest = one.hexDigest();
    Sha256 split;
    split.update(std::string_view(longText.data(), 60));
    split.update(std::string_view(longText.data() + 60, 68));
    REQUIRE(split.hexDigest() == digest);
    REQUIRE(digest.size() == 64);
}

TEST_CASE("file hashing is chunk-size independent and deterministic",
          "[hash]") {
    const auto dir = std::filesystem::temp_directory_path() /
                     ("mcaudit_hash_" +
                      std::to_string(static_cast<long>(::getpid())));
    std::filesystem::create_directories(dir);
    const auto path = dir / "seg.bin";
    std::string payload;
    for (int i = 0; i < 5000; ++i)
        payload.push_back(static_cast<char>(i & 0xff));
    std::ofstream out(path, std::ios::binary);
    out.write(payload.data(), static_cast<std::streamsize>(payload.size()));
    out.close();
    REQUIRE(out);

    auto a = hashFile(path, 64);
    auto b = hashFile(path, 4096);
    auto c = hashFile(path, 1 << 16);
    REQUIRE(a.has_value());
    REQUIRE(*a == *b);
    REQUIRE(*b == *c);

    Sha256 direct;
    direct.update(payload);
    REQUIRE(*a == direct.hexDigest());

    REQUIRE_FALSE(hashFile(dir / "missing.bin").has_value());
}
