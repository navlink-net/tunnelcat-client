cmake_minimum_required(VERSION 3.22.1)
project("snc_native")

add_library(snc_native SHARED native.c)
target_link_libraries(snc_native android log)
