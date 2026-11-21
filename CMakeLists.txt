cmake_minimum_required(VERSION 3.28)
project(relay_pipeline LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libavutil libswresample)
find_package(Catch2 3 REQUIRED)

add_library(relay_core
  src/frame_record.cpp
  src/clock_repair.cpp
  src/frame_decoder.cpp
  src/index_builder.cpp
  src/segment_writer.cpp
  src/stream_pipeline.cpp
)
target_include_directories(relay_core PUBLIC include)
target_link_libraries(relay_core PUBLIC PkgConfig::FFMPEG)

add_executable(relay_check app/relay_check.cpp)
target_link_libraries(relay_check PRIVATE relay_core)

add_executable(generate_fixtures tests/generate_fixtures.cpp)
target_link_libraries(generate_fixtures PRIVATE relay_core)

add_executable(pipeline_tests tests/pipeline_tests.cpp)
target_link_libraries(pipeline_tests PRIVATE relay_core Catch2::Catch2WithMain)

enable_testing()
include(CTest)
include(Catch)
catch_discover_tests(pipeline_tests)
