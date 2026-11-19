cmake_minimum_required(VERSION 3.28)
project(media_archive LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavcodec libavformat libavutil libswresample)

find_package(Catch2 3 REQUIRED)

add_library(marecover STATIC
  src/common.cpp
  src/packet_reader.cpp
  src/sequence_repair.cpp
  src/timestamp_repair.cpp
  src/audio_sync.cpp
  src/keyframe_index.cpp
  src/checkpoint_store.cpp
  src/transport_trace.cpp
  src/sample_generator.cpp
  src/report.cpp
  src/recovery_pipeline.cpp
)
target_include_directories(marecover PUBLIC
  ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(marecover PUBLIC PkgConfig::FFMPEG)
target_compile_options(marecover PRIVATE -Wall -Wextra -Wpedantic)

add_executable(media-archive src/main.cpp)
target_link_libraries(media-archive PRIVATE marecover)
target_compile_options(media-archive PRIVATE -Wall -Wextra -Wpedantic)

enable_testing()
add_executable(recovery-tests tests/recovery_tests.cpp)
target_link_libraries(recovery-tests PRIVATE marecover Catch2::Catch2WithMain)
target_compile_options(recovery-tests PRIVATE -Wall -Wextra -Wpedantic)

include(CTest)
include(Catch)
catch_discover_tests(recovery-tests)
