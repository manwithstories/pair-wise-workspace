#include "media/sha256.hpp"

#include <catch2/catch_test_macros.hpp>

#include <string>

using namespace mediaqc;

TEST_CASE("sha256 matches published vectors", "[checksum]") {
  // FIPS 180-4 test vectors.
  CHECK(to_hex(sha256(std::string_view(""))) ==
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  CHECK(to_hex(sha256(std::string_view("abc"))) ==
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  CHECK(to_hex(sha256(std::string_view(
            "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"))) ==
        "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
}

TEST_CASE("sha256 streaming equals one-shot across block boundaries", "[checksum]") {
  std::string data(200, 'x');
  for (std::size_t i = 0; i < data.size(); ++i) {
    data[i] = static_cast<char>('a' + (i % 26));
  }
  Sha256 hasher;
  std::size_t offset = 0;
  const std::size_t steps[] = {1, 55, 56, 63, 64, 65, 7};
  std::size_t step_index = 0;
  while (offset < data.size()) {
    std::size_t take = steps[step_index % 7];
    if (offset + take > data.size()) take = data.size() - offset;
    hasher.update(data.data() + offset, take);
    offset += take;
    ++step_index;
  }
  CHECK(hasher.finish() == sha256(data));
}

TEST_CASE("hex digests round-trip", "[checksum]") {
  Sha256Digest digest = sha256(std::string_view("round-trip"));
  std::string hex = to_hex(digest);
  REQUIRE(hex.size() == 64);
  auto parsed = parse_hex_digest(hex);
  REQUIRE(parsed.has_value());
  CHECK(*parsed == digest);
  CHECK_FALSE(parse_hex_digest("xyz").has_value());
  CHECK_FALSE(parse_hex_digest(std::string(63, 'a')).has_value());
}
