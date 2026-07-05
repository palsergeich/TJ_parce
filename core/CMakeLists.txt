# core/ — C++ ядро нормализатора ТЖ 1С: статическая библиотека tj_core,
# тонкий CLI tj-agent-cpp (участник bake-off) и C ABI tj_core_ffi (DLL).
#
# Сборка (из корня репозитория, внутри окружения vcvars64):
#   cmake -G "NMake Makefiles" -S core -B core/build
#   cmake --build core/build
# Артефакты: core/build/tj-agent-cpp.exe, core/build/tj_core_ffi.dll,
#            core/build/tj_ffi_selftest.exe
cmake_minimum_required(VERSION 3.20)
project(tj_core LANGUAGES CXX C)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
    set(CMAKE_BUILD_TYPE Release CACHE STRING "" FORCE)
endif()

if(MSVC)
    # Флаги релиза строго по bakeoff-protocol §2.1: /O2 /GL /arch:AVX2 /DNDEBUG + /LTCG.
    # (/Oi и /Ot включены в /O2; /fp:fast в протоколе нет — не добавляем.)
    # /utf-8 — не оптимизация: интерпретация исходников с кириллицей.
    add_compile_options(
        /O2 /GL /arch:AVX2 /utf-8
        "$<$<COMPILE_LANGUAGE:CXX>:/EHsc>"
    )
    add_compile_definitions(NOMINMAX _CRT_SECURE_NO_WARNINGS NDEBUG)
    add_link_options(/LTCG)
    # /O2|/GL конфликтуют с дефолтными флагами конфигураций — очищаем дубли
    foreach(lang C CXX)
        string(REPLACE "/O2" "" CMAKE_${lang}_FLAGS_RELEASE "${CMAKE_${lang}_FLAGS_RELEASE}")
    endforeach()
endif()

# --- библиотека ядра -------------------------------------------------------
add_library(tj_core STATIC
    src/parser.cpp
    src/pipeline.cpp
    src/util.cpp
)
target_include_directories(tj_core
    PUBLIC  ${CMAKE_CURRENT_SOURCE_DIR}/include
    PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/src
)

# --- CLI (участник bake-off; замена cpp_parse/count_contexts) ---------------
add_executable(tj-agent-cpp cli/main.cpp)
target_link_libraries(tj-agent-cpp PRIVATE tj_core)

# --- C ABI (FFI) -------------------------------------------------------------
option(TJ_BUILD_FFI "Собирать C ABI (tj_core_ffi.dll + selftest)" ON)
if(TJ_BUILD_FFI)
    add_library(tj_core_ffi SHARED ffi/tj_ffi.cpp)
    target_link_libraries(tj_core_ffi PRIVATE tj_core)
    target_include_directories(tj_core_ffi PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/ffi)
    target_compile_definitions(tj_core_ffi PRIVATE TJ_FFI_BUILD)

    add_executable(tj_ffi_selftest ffi/selftest.c)
    target_link_libraries(tj_ffi_selftest PRIVATE tj_core_ffi)
endif()
