cmake_minimum_required(VERSION 3.25)
project(timecode_tool LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra -Werror)

find_package(fmt 10.2 REQUIRED)
find_package(nlohmann_json 3.11 REQUIRED)
find_package(CLI11 2.4 REQUIRED)

add_library(timecode_core STATIC
  src/rate_table.cpp
  src/parser.cpp
)
target_include_directories(timecode_core PUBLIC
  ${CMAKE_CURRENT_SOURCE_DIR}/include
)
target_link_libraries(timecode_core PUBLIC
  fmt::fmt
  nlohmann_json::nlohmann_json
)

add_executable(timecode_tool app/main.cpp)
target_link_libraries(timecode_tool PRIVATE timecode_core CLI11::CLI11)

option(TIMECODE_BUILD_TESTS "Build Catch2 unit tests" ON)
if(TIMECODE_BUILD_TESTS)
  find_package(Catch2 3.5 REQUIRED)
  enable_testing()
  add_executable(timecode_tests tests/timecode_tests.cpp)
  target_link_libraries(timecode_tests PRIVATE timecode_core Catch2::Catch2WithMain)
  add_test(NAME timecode_tests COMMAND timecode_tests)
endif()
