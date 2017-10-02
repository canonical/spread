# -*- Mode: CMake; indent-tabs-mode: nil; tab-width: 4 -*-
#
# Copyright (C) 2015-2016 Canonical, Ltd.
#
# This program is free software; you can redistribute it and/or modify
# it under the terms of the GNU General Public License as published by
# the Free Software Foundation; version 3.
#
# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

cmake_minimum_required(VERSION 3.0)
add_library(snappypreload SHARED preload.c)
set_target_properties(snappypreload PROPERTIES
                      COMPILE_FLAGS "-DSNAPPY_LIBNAME=\\\"libsnappypreload.so\\\"")
target_link_libraries(snappypreload -ldl)
install(TARGETS snappypreload DESTINATION /usr/lib)
