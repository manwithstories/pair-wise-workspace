cmake_minimum_required(VERSION 3.22)
project(popfix LANGUAGES CXX)

# ---- 编译选项 ----------------------------------------------------------
option(POPFIX_ENABLE_TESTS "Build popfix unit tests" ON)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

if(MSVC)
  add_compile_options(/W4 /permissive-)
else()
  add_compile_options(-Wall -Wextra -Wpedantic)
endif()

# ---- 共享静态库（主程序与测试复用同一份实现）---------------------------
add_library(popfix_core STATIC
  src/wav_io.cpp
  src/clip_detector.cpp
  src/interpolator.cpp
)
target_include_directories(popfix_core PUBLIC src)
find_package(Threads REQUIRED)
target_link_libraries(popfix_core PUBLIC Threads::Threads)

# ---- 主程序 ------------------------------------------------------------
add_executable(popfix
  src/main.cpp
)
target_link_libraries(popfix PRIVATE popfix_core)

# ---- 测试 --------------------------------------------------------------
if(POPFIX_ENABLE_TESTS)
  enable_testing()
  add_executable(test_popfix tests/test_detector.cpp)
  target_link_libraries(test_popfix PRIVATE popfix_core)
  add_test(NAME popfix_unit_tests COMMAND test_popfix)
endif()

# ---- 安装入口 ----------------------------------------------------------
include(GNUInstallDirs)
install(TARGETS popfix RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
install(PROGRAMS scripts/run_demo.sh
  DESTINATION ${CMAKE_INSTALL_BINDIR} RENAME popfix-demo)
