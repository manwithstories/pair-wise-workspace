cmake_minimum_required(VERSION 3.25)
project(media_batch CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra -Werror=return-type)

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libavutil libswresample)

add_library(mbcore
  src/util.cpp
)
target_include_directories(mbcore PUBLIC src)
target_link_libraries(mbcore PUBLIC PkgConfig::FFMPEG)

add_executable(media_batch src/main.cpp)
target_link_libraries(media_batch PRIVATE mbcore)

enable_testing()
find_package(Catch2 3 QUIET)
if(Catch2_FOUND)
  add_executable(pipeline_tests tests/pipeline_tests.cpp)
  target_link_libraries(pipeline_tests PRIVATE mbcore Catch2::Catch2WithMain)
  include(Catch)
  catch_discover_tests(pipeline_tests)
else()
  message(STATUS "Catch2 not found; tests disabled")
endif()
