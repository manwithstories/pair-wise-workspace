add_executable(media_tests
  frame_transform_tests.cpp
  generator_tests.cpp
  keyframe_index_tests.cpp
  packet_tests.cpp
  pipeline_tests.cpp
  reorder_tests.cpp
  sha256_tests.cpp
  timestamp_tests.cpp
)
target_link_libraries(media_tests PRIVATE media_core Catch2::Catch2WithMain)

include(Catch)
catch_discover_tests(media_tests)
