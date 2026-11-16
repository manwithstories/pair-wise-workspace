cmake_minimum_required(VERSION 3.20)
project(lutgrade LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_executable(lutgrade
  src/main.cpp
  src/cli/Args.cpp
  src/cli/Commands.cpp
  src/cli/Demo.cpp
  src/color/ColorSpace.cpp
  src/image/FrameIO.cpp
  src/interp/Kernel.cpp
  src/lut/LutParser.cpp
  src/pipeline/CpuPath.cpp
  src/pipeline/FixedPath.cpp
  src/report/Histogram.cpp
  src/report/Json.cpp
)

target_include_directories(lutgrade PRIVATE src)

find_package(Threads REQUIRED)
target_link_libraries(lutgrade PRIVATE Threads::Threads)

enable_testing()

set(LG_OUT ${CMAKE_BINARY_DIR}/lg_test_out)

add_test(NAME demo COMMAND lutgrade demo --out-dir ${LG_OUT})
set_tests_properties(demo PROPERTIES
  PASS_REGULAR_EXPRESSION "grid=17 entries=4913 max_dev_r=0.000000")

add_test(NAME probe COMMAND lutgrade probe --lut ${LG_OUT}/identity17.cube)
set_tests_properties(probe PROPERTIES
  DEPENDS demo
  PASS_REGULAR_EXPRESSION
    "grid=17 domain_min=0.000000 domain_max=1.000000 format=cube order=r-fastest")

add_test(NAME apply
  COMMAND lutgrade apply --lut ${LG_OUT}/teal_orange33.cube
          --input ${LG_OUT}/gradient.ppm --output ${LG_OUT}/graded.ppm
          --report ${LG_OUT}/report.json)
set_tests_properties(apply PROPERTIES
  DEPENDS demo
  PASS_REGULAR_EXPRESSION "psnr_db=(inf|[6-9][0-9]|[0-9]{3,})")

add_test(NAME verify
  COMMAND lutgrade verify --lut ${LG_OUT}/teal_orange33.cube
          --frames ${LG_OUT}/frames --tolerance 0.002)
set_tests_properties(verify PROPERTIES
  DEPENDS demo
  PASS_REGULAR_EXPRESSION "frames=48 .*within_tolerance=yes")

add_test(NAME bad_args COMMAND lutgrade)
set_tests_properties(bad_args PROPERTIES WILL_FAIL TRUE)

add_test(NAME missing_lut COMMAND lutgrade probe --lut /nonexistent.cube)
set_tests_properties(missing_lut PROPERTIES WILL_FAIL TRUE)
