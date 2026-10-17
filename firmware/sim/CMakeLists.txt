# Renders the firmware UI (main/ui.c) on the development machine and saves
# PNG screenshots, so layout changes can be checked without the board.
#
#   idf.py reconfigure            # in firmware/, downloads LVGL once
#   cmake -B build -S sim && cmake --build build && build/dock_sim out/
cmake_minimum_required(VERSION 3.16)
project(dock_sim C)

set(LVGL_DIR ${CMAKE_CURRENT_SOURCE_DIR}/../managed_components/lvgl__lvgl)
if(NOT EXISTS ${LVGL_DIR}/lvgl.h)
    message(FATAL_ERROR "LVGL not found; run `idf.py reconfigure` in firmware/ first")
endif()

file(GLOB_RECURSE LVGL_SRC ${LVGL_DIR}/src/*.c)
add_library(lvgl STATIC ${LVGL_SRC})
target_include_directories(lvgl PUBLIC ${LVGL_DIR} ${CMAKE_CURRENT_SOURCE_DIR})
target_compile_definitions(lvgl PUBLIC LV_CONF_INCLUDE_SIMPLE)
target_compile_options(lvgl PRIVATE -w)

add_executable(dock_sim sim.c ../main/ui.c)
target_include_directories(dock_sim PRIVATE stubs ../main)
target_compile_definitions(dock_sim PRIVATE FONT_DIR="${CMAKE_CURRENT_SOURCE_DIR}/../main/fonts")
target_link_libraries(dock_sim lvgl)
