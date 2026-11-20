cmake_minimum_required(VERSION 3.28)
project(mediaqc LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

add_compile_options(-Wall -Wextra -Wpedantic)

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat>=60 libavcodec>=60 libavutil>=58 libswresample>=4)
find_package(Catch2 3 REQUIRED)

# Source files are appended per module below so each committed stage builds.
set(MEDIAQC_SOURCES
  src/track_graph.cpp
  src/fs_util.cpp
  src/timeline.cpp
  src/json.cpp)

add_library(mediaqc STATIC ${MEDIAQC_SOURCES})
target_include_directories(mediaqc PUBLIC include)
target_link_libraries(mediaqc PUBLIC PkgConfig::FFMPEG)

enable_testing()
