cmake_minimum_required(VERSION 3.28)
project(media_batch LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra -Wpedantic -Wshadow -Wconversion)

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libswresample libavutil)

find_package(Threads REQUIRED)

add_library(mbcore STATIC
  src/sha256.cpp
)
target_include_directories(mbcore PUBLIC src)
target_link_libraries(mbcore PUBLIC PkgConfig::FFMPEG Threads::Threads)
