cmake_minimum_required(VERSION 3.28)
project(media_repair LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT APPLE)
  message(WARNING "This project targets Apple macOS; other platforms are untested")
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libswscale libswresample libavutil)

add_library(media_core STATIC
  src/byte_io.cpp
  src/frame_pattern.cpp
  src/gap_estimator.cpp
  src/media_types.cpp
  src/metadata_codec.cpp
  src/packet_reader.cpp
  src/packet_writer.cpp
  src/reorder_buffer.cpp
  src/sha256.cpp
  src/timestamp_repair.cpp
)
target_include_directories(media_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(media_core PUBLIC PkgConfig::FFMPEG)
target_compile_options(media_core PRIVATE -Wall -Wextra -Wpedantic)

enable_testing()
find_package(Catch2 3 CONFIG REQUIRED)

add_subdirectory(tests)
