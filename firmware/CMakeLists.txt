# Wireless Serial — ESP32-C3 firmware
#
# Build:
#   . .idf/esp-idf/export.sh
#   cd firmware && idf.py set-target esp32c3 && idf.py build
#
# The target chip is ESP32-C3; `idf.py set-target esp32c3` is the supported
# way to select it (this project has no other-chip support).

cmake_minimum_required(VERSION 3.16)

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(wireless_serial)
