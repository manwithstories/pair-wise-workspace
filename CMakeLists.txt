cmake_minimum_required(VERSION 3.22)
project(pairwise_bake LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(bake_core STATIC
    framebuffer.cpp
    hash.cpp
    json.cpp
    kernels_blend.cpp
    kernels_geometry.cpp
    timeline.cpp
    time_map.cpp
    transition_kernel.cpp
)
target_include_directories(bake_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR})
target_compile_options(bake_core PRIVATE
    -O2 -Wall -Wextra -Werror -fno-fast-math -ffp-contract=off
)

add_executable(transition_bake main.cpp)
target_link_libraries(transition_bake PRIVATE bake_core)
target_compile_options(transition_bake PRIVATE
    -O2 -Wall -Wextra -Werror -fno-fast-math -ffp-contract=off
)

add_executable(kernel_tests tests/test_transitions.cpp)
target_link_libraries(kernel_tests PRIVATE bake_core)
target_compile_options(kernel_tests PRIVATE
    -O2 -Wall -Wextra -Werror -fno-fast-math -ffp-contract=off
)
