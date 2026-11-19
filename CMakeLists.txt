cmake_minimum_required(VERSION 3.28)
project(media_batch LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libswresample libavutil)

add_library(media_core STATIC
  src/checkpoint_index.cpp
  src/demo_generator.cpp
  src/error_model.cpp
  src/jitter_buffer.cpp
  src/manifest.cpp
  src/packet_reorder.cpp
  src/pipeline.cpp
  src/av_sync.cpp
  src/recovery.cpp
  src/subtitle.cpp
)
target_include_directories(media_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(media_core PUBLIC PkgConfig::FFMPEG)
target_compile_options(media_core PRIVATE -Wall -Wextra)

add_executable(media_batch src/main.cpp)
target_link_libraries(media_batch PRIVATE media_core)

include(CTest)
if(BUILD_TESTING)
  find_package(Catch2 3 REQUIRED)
  add_executable(pipeline_tests tests/pipeline_tests.cpp)
  target_link_libraries(pipeline_tests PRIVATE media_core Catch2::Catch2WithMain)
  include(Catch)
  catch_discover_tests(pipeline_tests)
endif()
