cmake_minimum_required(VERSION 3.28)
project(media_audit LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "" FORCE)
endif()

find_package(Threads REQUIRED)
find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat>=60
  libavcodec>=60
  libavutil>=58
  libswresample>=4)
find_package(Catch2 3 REQUIRED)

add_library(ma_core STATIC
  src/probe.cpp
  src/gop_analyzer.cpp
  src/damage_locator.cpp
  src/stream_pipeline.cpp
  src/sync_recovery.cpp
  src/fixture_gen.cpp
  src/report.cpp)
target_include_directories(ma_core PUBLIC src)
target_link_libraries(ma_core PUBLIC
  PkgConfig::FFMPEG
  Threads::Threads)

add_executable(media-audit src/main.cpp)
target_link_libraries(media-audit PRIVATE ma_core)

add_executable(ma_tests
  tests/test_clock.cpp
  tests/test_pipeline.cpp)
target_link_libraries(ma_tests PRIVATE ma_core Catch2::Catch2WithMain)

include(CTest)
include(Catch)
catch_discover_tests(ma_tests)
