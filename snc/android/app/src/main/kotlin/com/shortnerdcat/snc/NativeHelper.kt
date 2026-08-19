// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import java.io.FileDescriptor

object NativeHelper {
    init {
        System.loadLibrary("snc_native")
    }

    // clearCloexec clears FD_CLOEXEC so the fd is inherited across execve.
    @JvmStatic
    external fun clearCloexec(fd: Int)

    // forkExec forks and execs binary with the given environment, preserving tunFd
    // across exec. Android's ProcessBuilder closes all non-standard fds in the child;
    // this bypasses that restriction. Returns [pid, logReadFd], or [-1, -1] on error.
    @JvmStatic
    external fun forkExec(binary: String, env: Array<String>, tunFd: Int): IntArray

    // waitForPid blocks until the child process exits and returns its exit code.
    @JvmStatic
    external fun waitForPid(pid: Int): Int

    // killPid sends SIGTERM to the process.
    @JvmStatic
    external fun killPid(pid: Int)

    // sigkillPid sends SIGKILL to the process (immediate, no cleanup).
    @JvmStatic
    external fun sigkillPid(pid: Int)

    // fdToInt extracts the raw integer fd from a FileDescriptor via reflection.
    fun fdToInt(fd: FileDescriptor): Int {
        val field = FileDescriptor::class.java.getDeclaredField("descriptor")
        field.isAccessible = true
        return field.getInt(fd)
    }
}
