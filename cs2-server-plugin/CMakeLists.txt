cmake_minimum_required(VERSION 3.14)
project(cs2-server-plugin)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# Include FetchContent module
include(FetchContent)

# Fetch nlohmann/json
FetchContent_Declare(
    json
    GIT_REPOSITORY https://github.com/nlohmann/json.git
    GIT_TAG v3.12.0
)

# Fetch hl2sdk
FetchContent_Declare(
    hl2sdk
    GIT_REPOSITORY https://github.com/alliedmodders/hl2sdk.git
    GIT_TAG 9ab16fa9fcdeeb30565dfdbf6fbb312356978a0b
)

# Make dependencies available
FetchContent_MakeAvailable(json hl2sdk)

# Define compile flags
add_compile_options(
    -Wall
    -fPIC
    -DPOSIX=1
    -DNO_MALLOC_OVERRIDE=1
    -DCOMPILER_GCC=1
    -DPLATFORM_POSIX=1
    -DX64BITS=1
    -DPLATFORM_64BITS=1
    -DLINUX=1
    -Dstricmp=strcasecmp
)

# Create shared library
add_library(server SHARED
    main.cpp
    ${hl2sdk_SOURCE_DIR}/tier1/convar.cpp
)

# Include directories
target_include_directories(server PRIVATE
    ${json_SOURCE_DIR}/include
    ${hl2sdk_SOURCE_DIR}/public
    ${hl2sdk_SOURCE_DIR}/public/tier0
    ${hl2sdk_SOURCE_DIR}/public/tier1
)

# Link directories
target_link_directories(server PRIVATE
    ${hl2sdk_SOURCE_DIR}/lib/linux64
)

# Link libraries
target_link_libraries(server PRIVATE
    dl
    ${hl2sdk_SOURCE_DIR}/lib/linux64/libtier0.so
    ${hl2sdk_SOURCE_DIR}/lib/linux64/tier1.a
)

# Set output name and ensure all symbols are visible (match Makefile behavior)
set_target_properties(server PROPERTIES
    PREFIX "lib"
    OUTPUT_NAME "server"
    SUFFIX ".so"
    CXX_VISIBILITY_PRESET default
    VISIBILITY_INLINES_HIDDEN OFF
)
