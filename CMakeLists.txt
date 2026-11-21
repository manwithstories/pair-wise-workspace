cmake_minimum_required(VERSION 3.28)
project(media_pipeline LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra -Wpedantic)

# ---- FFmpeg (libavformat / libavcodec / libswresample / libavutil) ----
find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libswresample libavutil)

# ---- 静态库：各模块按批次加入 ----
add_library(media_core STATIC
  src/error.cpp
  src/json.cpp
  src/file_util.cpp
  src/wav_reader.cpp
  src/silence.cpp
  src/segmenter.cpp
  src/fingerprint.cpp
)
target_include_directories(media_core PUBLIC include)
target_link_libraries(media_core PUBLIC PkgConfig::FFMPEG)

option(MEDIA_BUILD_TESTS "Build Catch2 unit tests" ON)
