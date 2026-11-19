#include <catch2/catch_test_macros.hpp>

#include "error.hpp"

TEST_CASE("error codes map to the documented exit codes", "[error]") {
  using media_restore::ErrorCode;
  CHECK(media_restore::exit_code_for(ErrorCode::usage) == 1);
  CHECK(media_restore::exit_code_for(ErrorCode::json_parse) == 1);
  CHECK(media_restore::exit_code_for(ErrorCode::index_order) == 2);
  CHECK(media_restore::exit_code_for(ErrorCode::dependency_cycle) == 3);
  CHECK(media_restore::exit_code_for(ErrorCode::audio_unsupported_layout) == 3);
}

TEST_CASE("diagnostics render stable fields", "[error]") {
  media_restore::Diagnostic diag;
  diag.code = media_restore::ErrorCode::index_negative_offset;
  diag.message = "offset must not be negative";
  diag.location.file = "media/reel.h264";
  diag.location.byte_offset = 42;
  diag.location.track = "reel_video";
  const std::string text = media_restore::format_diagnostic(diag);
  CHECK(text.find("code=E_INDEX_NEGATIVE_OFFSET") != std::string::npos);
  CHECK(text.find("track=reel_video") != std::string::npos);
  CHECK(text.find("offset=42") != std::string::npos);
}
