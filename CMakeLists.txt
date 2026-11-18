cmake_minimum_required(VERSION 3.16)
project(motion VERSION 1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra)

add_library(motion_core STATIC
  src/ppm.cpp
  src/diff.cpp
)
target_include_directories(motion_core PUBLIC src)

add_executable(motion src/main.cpp)
target_link_libraries(motion PRIVATE motion_core)

enable_testing()
