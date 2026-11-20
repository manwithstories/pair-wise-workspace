cmake_minimum_required(VERSION 3.28)
project(media_batch LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra -Wpedantic -Wshadow -Wconversion)

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libswresample libavutil)

find_package(Threads REQUIRED)

add_library(mbcore STATIC
  src/sha256.cpp
  src/capture.cpp
  src/rtp_ingest.cpp
  src/timeline.cpp
  src/track_graph.cpp
  src/segmenter.cpp
  src/silence.cpp
  src/fingerprint.cpp
  src/fixture.cpp
  src/pipeline.cpp
)
target_include_directories(mbcore PUBLIC src)
target_link_libraries(mbcore PUBLIC PkgConfig::FFMPEG Threads::Threads)

add_executable(media_batch src/main.cpp)
target_link_libraries(media_batch PRIVATE mbcore)

option(MB_BUILD_TESTS "Build Catch2 unit tests" ON)
if(MB_BUILD_TESTS)
  enable_testing()
  find_package(Catch2 3.7 REQUIRED)
  add_executable(media_batch_tests tests/pipeline_test.cpp)
  target_link_libraries(media_batch_tests PRIVATE mbcore Catch2::Catch2WithMain)
  add_test(NAME pipeline COMMAND media_batch_tests)
endif()
