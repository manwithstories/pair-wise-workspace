cmake_minimum_required(VERSION 3.28)
project(mediacore_audit LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "" FORCE)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libswresample libavutil)
find_package(Catch2 3 REQUIRED)
find_package(Threads REQUIRED)

add_library(mediacore STATIC
  src/version.cpp
  src/time.cpp
  src/error.cpp
  src/pathutil.cpp
  src/json.cpp
  src/json_parse.cpp
  src/manifest.cpp
  src/hls_parser.cpp
  src/dash_template.cpp
  src/dash_expand.cpp
  src/dash_parser.cpp
  src/serialize.cpp
  src/materialize.cpp
  src/roundtrip.cpp
  src/probe.cpp
  src/keyframe_analysis.cpp
)
target_include_directories(mediacore PUBLIC
  ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(mediacore PUBLIC
  PkgConfig::FFMPEG Threads::Threads)

enable_testing()
include(CTest)

add_executable(test_core tests/test_core.cpp)
target_link_libraries(test_core PRIVATE mediacore Catch2::Catch2WithMain)
add_test(NAME core COMMAND test_core)

add_executable(test_manifest tests/test_manifest.cpp)
target_link_libraries(test_manifest PRIVATE
  mediacore Catch2::Catch2WithMain)
add_test(NAME manifest COMMAND test_manifest)

add_executable(test_probe tests/test_probe.cpp)
target_link_libraries(test_probe PRIVATE
  mediacore Catch2::Catch2WithMain)
add_test(NAME probe COMMAND test_probe)
