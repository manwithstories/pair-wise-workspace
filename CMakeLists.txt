cmake_minimum_required(VERSION 3.28)
project(media_replay LANGUAGES CXX VERSION 0.1.0)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

if(APPLE)
  list(PREPEND CMAKE_PREFIX_PATH /opt/homebrew /usr/local)
  list(PREPEND CMAKE_LIBRARY_PATH /opt/homebrew/lib /usr/local/lib)
endif()

add_compile_options(-Wall -Wextra -Wno-unused-parameter)

# ---- FFmpeg (compatible with FFmpeg 7 and 8) --------------------------------
find_package(PkgConfig QUIET)
set(_ff_targets "")
if(PkgConfig_FOUND)
  pkg_check_modules(PC_AVFORMAT QUIET IMPORTED_TARGET libavformat)
  pkg_check_modules(PC_AVCODEC  QUIET IMPORTED_TARGET libavcodec)
  pkg_check_modules(PC_SWRES    QUIET IMPORTED_TARGET libswresample)
  pkg_check_modules(PC_AVUTIL   QUIET IMPORTED_TARGET libavutil)
  if(PC_AVCODEC_FOUND AND PC_AVUTIL_FOUND)
    add_library(media_ffmpeg INTERFACE)
    target_link_libraries(media_ffmpeg INTERFACE
      PkgConfig::PC_AVFORMAT PkgConfig::PC_AVCODEC
      PkgConfig::PC_SWRES PkgConfig::PC_AVUTIL)
    set(_ff_targets media_ffmpeg)
  endif()
endif()
if(NOT _ff_targets)
  find_path(FF_INCLUDE_DIR libavcodec/avcodec.h
    PATHS /opt/homebrew/include /usr/local/include /usr/include)
  foreach(c avformat avcodec swresample avutil)
    find_library(FF_${c}_LIB ${c}
      PATHS /opt/homebrew/lib /usr/local/lib /usr/lib)
    list(APPEND _ff_libs ${FF_${c}_LIB})
  endforeach()
  add_library(media_ffmpeg INTERFACE)
  target_include_directories(media_ffmpeg INTERFACE ${FF_INCLUDE_DIR})
  target_link_libraries(media_ffmpeg INTERFACE ${_ff_libs})
  set(_ff_targets media_ffmpeg)
endif()

# ---- Catch2 ------------------------------------------------------------------
find_package(Catch2 3 QUIET)

# ---- Library -----------------------------------------------------------------
add_library(media_core STATIC
  src/checksum.cpp
  src/sample_generator.cpp
  src/sample_fixtures.cpp
  src/stream_codec.cpp
  src/stream_reader.cpp
  src/stream_index_text.cpp
  src/stream_index.cpp
  src/network_replay.cpp
  src/jitter_buffer.cpp
  src/recovery.cpp
  src/cli.cpp)
# Modules are appended to media_core in later, independently compilable batches.
# Modules are appended to media_core in later, independently compilable batches.
target_include_directories(media_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(media_core PUBLIC ${_ff_targets})

add_executable(media_replay src/main.cpp)
target_link_libraries(media_replay PRIVATE media_core)

# ---- Tests -------------------------------------------------------------------
enable_testing()
if(Catch2_FOUND AND EXISTS ${CMAKE_CURRENT_SOURCE_DIR}/tests/pipeline_tests.cpp)
  add_executable(media_tests tests/pipeline_tests.cpp)
  target_link_libraries(media_tests PRIVATE media_core Catch2::Catch2WithMain)
  include(CTest)
  include(${Catch2_DIR}/Catch.cmake)
  catch_discover_tests(media_tests)
endif()
