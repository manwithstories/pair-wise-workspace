cmake_minimum_required(VERSION 3.20)
project(h264inspect LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_library(h264lib STATIC
  src/bit_reader.cpp
)
target_include_directories(h264lib PUBLIC src)

enable_testing()

add_executable(test_bit_reader tests/test_bit_reader.cpp tests/test_framework.cpp)
target_link_libraries(test_bit_reader PRIVATE h264lib)
add_test(NAME bit_reader COMMAND test_bit_reader)
