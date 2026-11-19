#include <catch2/catch_test_macros.hpp>

#include "media_clock.hpp"
#include "sha256.hpp"
#include "util.hpp"

using namespace mediaqc;

TEST_CASE("rescale_ticks is exact and integer-only") {
  // 1/10240 -> 1/90000 for one video frame at 10 fps.
  REQUIRE(rescale_ticks(1024, Rational{1, 10240}, Rational{1, 90000}) == 9000);
  REQUIRE(rescale_ticks(0, Rational{1, 10240}, Rational{1, 90000}) == 0);
  REQUIRE(rescale_ticks(-1024, Rational{1, 48000}, Rational{1, 90000}) == -1920);
}

TEST_CASE("rescale_ticks passes NOPTS through") {
  REQUIRE(rescale_ticks(AV_NOPTS_VALUE, Rational{1, 1000}, Rational{1, 90000}) ==
          AV_NOPTS_VALUE);
}

TEST_CASE("round trip does not drift over many ticks") {
  const Rational src{1, 48000};
  const Rational dst{1, 90000};
  int64_t max_err = 0;
  for (int64_t t = 0; t < 48000 * 60; t += 1024) {
    const int64_t there = rescale_ticks(t, src, dst);
    const int64_t back = rescale_ticks(there, dst, src);
    const int64_t err = back > t ? back - t : t - back;
    if (err > max_err) max_err = err;
  }
  // One dst tick is 48000/90000 of a src tick, so the round trip may move by
  // at most half a dst tick expressed in src ticks.
  REQUIRE(max_err <= 1);
}

TEST_CASE("wrap policy defaults to a 32-bit rollover period") {
  WrapPolicy p;
  REQUIRE(p.period() == kRolloverPeriod);
  REQUIRE(p.threshold() == kRolloverPeriod / 2);
  WrapPolicy q;
  q.period_ticks = 1000;
  REQUIRE(q.period() == 1000);
  REQUIRE(q.threshold() == 500);
}

TEST_CASE("seconds_to_ticks uses the supplied base") {
  REQUIRE(seconds_to_ticks(1, Rational{1, 90000}) == 90000);
  REQUIRE(seconds_to_ticks(0, Rational{1, 1000}) == 0);
  REQUIRE(canonical_millis(90000) == 1000);
}

TEST_CASE("sha256 matches known vectors") {
  Sha256 a;
  a.update("abc");
  REQUIRE(a.hex_digest() ==
          "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  Sha256 b;
  b.update("");
  REQUIRE(b.hex_digest() ==
          "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  Sha256 c;
  c.update(std::string(1000, 'a'));
  REQUIRE(c.hex_digest() ==
          "41edece42d63e8d9bf515a9ba6932e1c20cbc9f5a5d134645adb5db1b9737ea3");
}

TEST_CASE("json writer escapes and types values") {
  JsonWriter w;
  w.begin_object().key("a").value("x\"y").key("n").value(int64_t{-3}).key("ok").value(true).end_object();
  REQUIRE(w.str() == R"({"a":"x\"y","n":-3,"ok":true})");
}
