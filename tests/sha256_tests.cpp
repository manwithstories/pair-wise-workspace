#include <catch2/catch_test_macros.hpp>

#include "sha256.hpp"

using mfs::Sha256;

TEST_CASE("SHA-256 matches FIPS 180-2 vectors", "[sha256]") {
  SECTION("empty input") {
    CHECK(Sha256::hex(Sha256::hash(std::string{})) ==
          "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  }
  SECTION("abc") {
    CHECK(Sha256::hex(Sha256::hash(std::string{"abc"})) ==
          "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  }
  SECTION("two-block message") {
    CHECK(Sha256::hex(Sha256::hash(std::string{
              "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"})) ==
          "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
  }
  SECTION("incremental updates agree with one-shot") {
    const std::string message =
        "The quick brown fox jumps over the lazy dog, repeatedly and then some.";
    Sha256 incremental;
    for (char c : message) {
      incremental.update(&c, 1);
    }
    CHECK(incremental.finish() == Sha256::hash(message));
  }
}
