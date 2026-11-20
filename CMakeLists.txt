cmake_minimum_required(VERSION 3.31)
project(media_tool LANGUAGES CXX)

# ---- 全局编译选项：C++20，无外部服务依赖，纯终端 ----
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "" FORCE)
endif()

if(CMAKE_CXX_COMPILER_ID MATCHES "Clang|GNU")
  add_compile_options(-Wall -Wextra -Wno-deprecated-declarations)
endif()

# ---- FFmpeg（7.1 及以上 API；本机 9.x 向后兼容）----
find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat>=60
  libavcodec>=60
  libavutil>=58
  libswresample>=4
)

# ---- Catch2 3.x ----
find_package(Catch2 3 REQUIRED)

# ---- 核心静态库：统一时间基准 / 错误模型贯穿全部模块 ----
add_library(mediacore STATIC
  src/core_types.cpp
  src/path_guard.cpp
  src/wav_demuxer.cpp
  src/fixture.cpp
  src/track_mapper.cpp
  src/quality.cpp
  src/remux_muxer.cpp
  src/manifest.cpp
)
target_include_directories(mediacore PUBLIC
  ${CMAKE_CURRENT_SOURCE_DIR}/include
)
target_link_libraries(mediacore PUBLIC PkgConfig::FFMPEG)

enable_testing()
