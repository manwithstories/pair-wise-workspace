cmake_minimum_required(VERSION 3.28)
project(mediaqc LANGUAGES CXX VERSION 1.0.0)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libavutil libswresample)

# Sources are added per module; keep the list sorted so patches stay small.
set(MEDIAQC_SOURCES
  src/config.cpp
  src/media_error.cpp
  src/mp4_structure.cpp
  src/scanner.cpp
  src/resume_store.cpp
  src/fixtures.cpp
  src/index_writer.cpp
  src/pipeline.cpp
  src/verifier.cpp
  src/sha256.cpp
  src/timeline.cpp
  src/util.cpp
)

add_library(mediaqc_core STATIC ${MEDIAQC_SOURCES})
target_include_directories(mediaqc_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/src)
target_link_libraries(mediaqc_core PUBLIC PkgConfig::FFMPEG)
target_compile_options(mediaqc_core PRIVATE -Wall -Wextra -Wpedantic)

add_executable(mediaqc src/main.cpp)
target_link_libraries(mediaqc PRIVATE mediaqc_core)
target_compile_options(mediaqc PRIVATE -Wall -Wextra -Wpedantic)

include(CTest)
if(BUILD_TESTING)
  find_package(Catch2 3 REQUIRED)
  list(APPEND CMAKE_MODULE_PATH ${CMAKE_CURRENT_SOURCE_DIR}/cmake)
  include(Catch)

  set(MEDIAQC_TEST_TARGETS)
  foreach(test_name clock timeline structure resume pipeline)
    add_executable(${test_name}_tests tests/${test_name}_tests.cpp)
    target_link_libraries(${test_name}_tests PRIVATE mediaqc_core Catch2::Catch2WithMain)
    target_compile_options(${test_name}_tests PRIVATE -Wall -Wextra)
    catch_discover_tests(${test_name}_tests)
  endforeach()
endif()
