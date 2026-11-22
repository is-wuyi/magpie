# pf-serve: privacy-filter.cpp (PF_SRC, patched by build.sh) and the
# process magpie talks to.
cmake_minimum_required(VERSION 3.21)
project(pf-serve C CXX)

set(PF_SRC "" CACHE PATH "a privacy-filter.cpp checkout")
if (NOT PF_SRC)
    message(FATAL_ERROR "set PF_SRC, or run build.sh")
endif()
option(PF_SERVE_DIAGNOSTICS "Build the backend tests and model tap tool" OFF)
set(PF_BUILD_TOOLS ${PF_SERVE_DIAGNOSTICS} CACHE BOOL "" FORCE)
set(PF_BUILD_TESTS OFF CACHE BOOL "" FORCE)
add_subdirectory(${PF_SRC} pf)

if (PF_SERVE_DIAGNOSTICS)
    find_package(Threads REQUIRED)
    add_executable(test-backend-ops ${PF_SRC}/ggml/tests/test-backend-ops.cpp)
    target_link_libraries(test-backend-ops PRIVATE ggml Threads::Threads)
    target_compile_features(test-backend-ops PRIVATE cxx_std_17)
    target_compile_options(test-backend-ops PRIVATE -UNDEBUG)
endif()

add_executable(pf-serve pf-serve.cpp)
target_link_libraries(pf-serve PRIVATE pf)
target_compile_features(pf-serve PRIVATE cxx_std_17)
