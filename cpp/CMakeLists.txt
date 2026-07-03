cmake_minimum_required(VERSION 3.15)
project(count_contexts CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# Оптимизации компилятора
if(MSVC)
    set(CMAKE_CXX_FLAGS_RELEASE "${CMAKE_CXX_FLAGS_RELEASE} /O2 /Ob2 /Oi /Ot /GL /fp:fast /arch:AVX2")
    set(CMAKE_EXE_LINKER_FLAGS_RELEASE "${CMAKE_EXE_LINKER_FLAGS_RELEASE} /LTCG /OPT:REF /OPT:ICF")
else()
    set(CMAKE_CXX_FLAGS_RELEASE "${CMAKE_CXX_FLAGS_RELEASE} -O3 -march=native -mavx2 -flto -ffast-math")
endif()

add_executable(count_contexts count_contexts.cpp)

# Линковка с pthread на Linux
if(UNIX)
    target_link_libraries(count_contexts pthread)
endif()

