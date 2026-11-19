cmake_minimum_required(VERSION 3.28)
project(media_deliver LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra -Wno-deprecated-declarations)

find_package(Catch2 3 REQUIRED)
find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libavutil libswresample)

add_library(md_core STATIC
  src/time_model.cpp
  src/packet.cpp
  src/manifest.cpp
  src/clock_repair.cpp
  src/stream_pipeline.cpp
  src/index_builder.cpp
  src/quality_report.cpp
  src/demuxer.cpp
  src/generator.cpp
  src/gen_patterns.cpp
  src/ffmpeg_util.cpp
  src/decoder.cpp)
target_include_directories(md_core PUBLIC include)
target_link_libraries(md_core PUBLIC PkgConfig::FFMPEG)
find_package(Threads REQUIRED)
target_link_libraries(md_core PUBLIC Threads::Threads)

add_executable(media-deliver src/main.cpp)
target_link_libraries(media-deliver PRIVATE md_core)

add_executable(repair_tests tests/repair_tests.cpp)
target_link_libraries(repair_tests PRIVATE md_core Catch2::Catch2WithMain)

include(CTest)
include(Catch)
catch_discover_tests(repair_tests)
