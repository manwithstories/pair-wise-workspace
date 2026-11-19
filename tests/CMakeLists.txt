add_executable(media_tests
  packet_tests.cpp
  sha256_tests.cpp
)
target_link_libraries(media_tests PRIVATE media_core Catch2::Catch2WithMain)

include(Catch)
catch_discover_tests(media_tests)
