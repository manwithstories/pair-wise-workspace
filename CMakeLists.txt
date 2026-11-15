cmake_minimum_required(VERSION 3.20)

project(h264scan LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(h264core
    src/analyzer.cpp
    src/bit_reader.cpp
    src/fixture_gen.cpp
    src/gop_report.cpp
    src/h264_headers.cpp
    src/json_writer.cpp
    src/nal_scan.cpp
    src/picture_assembler.cpp
)

target_include_directories(h264core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/include)

if(MSVC)
    target_compile_options(h264core PRIVATE /W4 /permissive-)
else()
    target_compile_options(h264core PRIVATE -Wall -Wextra -Wpedantic -Wshadow)
endif()

add_executable(h264scan src/main.cpp)
target_link_libraries(h264scan PRIVATE h264core)

if(MSVC)
    target_compile_options(h264scan PRIVATE /W4 /permissive-)
else()
    target_compile_options(h264scan PRIVATE -Wall -Wextra -Wpedantic -Wshadow)
endif()
