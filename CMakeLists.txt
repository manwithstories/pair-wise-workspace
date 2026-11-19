cmake_minimum_required(VERSION 3.25)
project(media_restore VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

# Homebrew is not always on CMake's default search path, especially on
# Apple Silicon when CMake is installed outside /opt/homebrew.
if(EXISTS "/opt/homebrew")
  list(APPEND CMAKE_PREFIX_PATH "/opt/homebrew")
endif()
if(EXISTS "/usr/local")
  list(APPEND CMAKE_PREFIX_PATH "/usr/local")
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libswresample libavutil)

find_package(Catch2 3 QUIET CONFIG)

add_library(media_restore_core STATIC
  src/error.cpp
  src/json.cpp
  src/rational.cpp
  src/hash.cpp
  src/file_io.cpp
  src/manifest.cpp
  src/bitstream.cpp
  src/timeline.cpp
  src/sha256.cpp
  src/audio_restore.cpp
  src/color_metadata.cpp
  src/fixture.cpp
  src/restore_engine.cpp
)
target_include_directories(media_restore_core PUBLIC include)
target_link_libraries(media_restore_core PUBLIC PkgConfig::FFMPEG)
target_compile_options(media_restore_core PRIVATE -Wall -Wextra -Wpedantic)

add_executable(media-restore src/main.cpp)
target_link_libraries(media-restore PRIVATE media_restore_core)
target_compile_options(media-restore PRIVATE -Wall -Wextra -Wpedantic)

if(Catch2_FOUND)
  enable_testing()
  add_executable(restore_tests tests/restore_tests.cpp)
  target_link_libraries(restore_tests PRIVATE media_restore_core Catch2::Catch2WithMain)
  target_compile_options(restore_tests PRIVATE -Wall -Wextra -Wpedantic)
  add_test(NAME restore_tests COMMAND restore_tests)

  # End-to-end coverage: generate a fixture archive, restore it and verify the
  # result through the real CLI, exactly as the acceptance run does.
  add_test(NAME cli_acceptance
           COMMAND ${CMAKE_COMMAND}
                   -DBINARY=$<TARGET_FILE:media-restore>
                   -DWORK_DIR=${CMAKE_CURRENT_BINARY_DIR}/acceptance
                   -P ${CMAKE_CURRENT_SOURCE_DIR}/tests/acceptance.cmake)
endif()
