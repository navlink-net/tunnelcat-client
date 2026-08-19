// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

#include <jni.h>
#include <fcntl.h>
#include <unistd.h>
#include <stdlib.h>
#include <string.h>
#include <signal.h>
#include <sys/wait.h>
#include <sys/resource.h>

JNIEXPORT jint JNICALL
Java_com_shortnerdcat_snc_NativeHelper_fdToInt(JNIEnv *env, jclass cls, jobject fd_obj);

// clearCloexec clears FD_CLOEXEC so the fd survives execve.
JNIEXPORT void JNICALL
Java_com_shortnerdcat_snc_NativeHelper_clearCloexec(JNIEnv *env, jclass cls, jint fd) {
    int flags = fcntl((int)fd, F_GETFD, 0);
    if (flags >= 0)
        fcntl((int)fd, F_SETFD, flags & ~FD_CLOEXEC);
}

// forkExec forks and execs binary with the given env, preserving tunFd across exec.
// Android's ProcessBuilder closes all fds >2 in the child; this bypasses that.
// Returns int[2] = {pid, logReadFd} on success, or int[1] = {-1} on error.
// logReadFd is the read end of a pipe connected to child's stdout+stderr.
JNIEXPORT jintArray JNICALL
Java_com_shortnerdcat_snc_NativeHelper_forkExec(
        JNIEnv *env, jclass cls,
        jstring jBinary,
        jobjectArray jEnv,
        jint tunFd) {

    jintArray result = (*env)->NewIntArray(env, 2);

    const char *binary = (*env)->GetStringUTFChars(env, jBinary, NULL);

    int envLen = (*env)->GetArrayLength(env, jEnv);
    char **envp = (char **)malloc((envLen + 1) * sizeof(char *));
    for (int i = 0; i < envLen; i++) {
        jstring s = (jstring)(*env)->GetObjectArrayElement(env, jEnv, i);
        envp[i] = (char *)(*env)->GetStringUTFChars(env, s, NULL);
        (*env)->DeleteLocalRef(env, s);
    }
    envp[envLen] = NULL;

    // argv = {binary, NULL}  — no args; config is via env vars
    char *argv[] = {(char *)binary, NULL};

    int pipefd[2];
    if (pipe(pipefd) < 0) {
        free(envp);
        (*env)->ReleaseStringUTFChars(env, jBinary, binary);
        jint err[] = {-1, -1};
        (*env)->SetIntArrayRegion(env, result, 0, 2, err);
        return result;
    }

    pid_t pid = fork();
    if (pid < 0) {
        close(pipefd[0]);
        close(pipefd[1]);
        free(envp);
        (*env)->ReleaseStringUTFChars(env, jBinary, binary);
        jint err[] = {-1, -1};
        (*env)->SetIntArrayRegion(env, result, 0, 2, err);
        return result;
    }

    if (pid == 0) {
        // Child: redirect stdout and stderr to pipe write end.
        dup2(pipefd[1], 1);
        dup2(pipefd[1], 2);

        // Close all fds except stdin(0), stdout(1), stderr(2) and tunFd.
        struct rlimit rl;
        int maxFd = 1024;
        if (getrlimit(RLIMIT_NOFILE, &rl) == 0 && rl.rlim_cur != RLIM_INFINITY)
            maxFd = (int)rl.rlim_cur;
        for (int fd = 3; fd < maxFd; fd++) {
            if (fd != tunFd)
                close(fd);
        }
        // tunFd must not have FD_CLOEXEC (cleared by caller before fork).
        execve(binary, argv, envp);
        _exit(127); // exec failed
    }

    // Parent: close write end of pipe (child owns it now).
    close(pipefd[1]);

    // Release resources and return {pid, readFd}.
    for (int i = 0; i < envLen; i++) {
        jstring s = (jstring)(*env)->GetObjectArrayElement(env, jEnv, i);
        (*env)->ReleaseStringUTFChars(env, s, envp[i]);
        (*env)->DeleteLocalRef(env, s);
    }
    free(envp);
    (*env)->ReleaseStringUTFChars(env, jBinary, binary);

    jint ok[] = {(jint)pid, (jint)pipefd[0]};
    (*env)->SetIntArrayRegion(env, result, 0, 2, ok);
    return result;
}

// waitForPid blocks until the child exits and returns its exit code.
JNIEXPORT jint JNICALL
Java_com_shortnerdcat_snc_NativeHelper_waitForPid(JNIEnv *env, jclass cls, jint pid) {
    int status = 0;
    waitpid((pid_t)pid, &status, 0);
    if (WIFEXITED(status))  return WEXITSTATUS(status);
    if (WIFSIGNALED(status)) return -(int)WTERMSIG(status);
    return -1;
}

// killPid sends SIGTERM to the process.
JNIEXPORT void JNICALL
Java_com_shortnerdcat_snc_NativeHelper_killPid(JNIEnv *env, jclass cls, jint pid) {
    kill((pid_t)pid, SIGTERM);
}

// sigkillPid sends SIGKILL to the process (no cleanup, immediate termination).
JNIEXPORT void JNICALL
Java_com_shortnerdcat_snc_NativeHelper_sigkillPid(JNIEnv *env, jclass cls, jint pid) {
    kill((pid_t)pid, SIGKILL);
}
