cmake_minimum_required(VERSION 3.28)
project(media_deliver LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavcodec libavformat libswresample libavutil)
find_package(Catch2 3 REQUIRED)

add_library(mdcore STATIC
  src/common.cpp
  src/capture_gen.cpp
  src/capture_read.cpp
  src/timeline.cpp
  src/segmenter.cpp
  src/silence.cpp
  src/fingerprint.cpp
  src/checkpoint.cpp
  src/pipeline.cpp
  src/pipeline_io.cpp
  src/pipeline_finalize.cpp
  src/sha256.cpp
  src/util.cpp)
target_include_directories(mdcore PUBLIC src)
target_link_libraries(mdcore PUBLIC PkgConfig::FFMPEG)

add_executable(media-deliver src/main.cpp)
target_link_libraries(media-deliver PRIVATE mdcore)

enable_testing()
add_executable(md_tests tests/pipeline_test.cpp)
target_link_libraries(md_tests PRIVATE mdcore Catch2::Catch2WithMain)
add_test(NAME pipeline_tests COMMAND md_tests)
