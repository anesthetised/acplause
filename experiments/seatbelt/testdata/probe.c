// Disposable native probes for ADR 0025. Reads synthetic fixtures only and
// never prints file contents or process environment bytes.
#include <errno.h>
#include <fcntl.h>
#include <arpa/inet.h>
#include <libproc.h>
#include <mach/mach.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/sysctl.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

static int network_probe(const char *kind, const char *endpoint) {
    int local = !strcmp(kind, "unix");
    int ipv6 = strstr(kind, "6") != NULL;
    int udp = !strncmp(kind, "udp", 3);
    int fd = socket(local ? AF_UNIX : ipv6 ? AF_INET6 : AF_INET,
                    udp ? SOCK_DGRAM : SOCK_STREAM, 0);
    if (fd < 0) return -1;
    struct timeval timeout = {.tv_sec = 1};
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    struct sockaddr_un un = {.sun_family = AF_UNIX};
    struct sockaddr_in v4 = {.sin_family = AF_INET};
    struct sockaddr_in6 v6 = {.sin6_family = AF_INET6};
    struct sockaddr *address;
    socklen_t size;
    if (local) {
        if (strlen(endpoint) >= sizeof(un.sun_path)) { close(fd); errno = ENAMETOOLONG; return -1; }
        strcpy(un.sun_path, endpoint);
        address = (struct sockaddr *)&un;
        size = sizeof(un);
    } else if (ipv6) {
        v6.sin6_addr = in6addr_loopback;
        v6.sin6_port = htons((unsigned short)atoi(endpoint));
        address = (struct sockaddr *)&v6;
        size = sizeof(v6);
    } else {
        v4.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        v4.sin_port = htons((unsigned short)atoi(endpoint));
        address = (struct sockaddr *)&v4;
        size = sizeof(v4);
    }
    int result = connect(fd, address, size);
    char value = 'x';
    if (!result) result = (int)write(fd, &value, 1);
    if (result >= 0) result = (int)read(fd, &value, 1);
    int saved = errno;
    close(fd);
    errno = saved;
    return result;
}

// The worker self-expires and also obeys an invocation-owned lease file. The
// harness never kills a PID obtained from a writable file or process listing.
static int worker(const char *outside, const char *workspace) {
    alarm(5);
    close(STDIN_FILENO);
    close(STDOUT_FILENO);
    close(STDERR_FILENO);
    char lease[4096], ready[4096], heartbeat[4096], done[4096];
    snprintf(lease, sizeof(lease), "%s/lease", workspace);
    snprintf(ready, sizeof(ready), "%s/ready", workspace);
    snprintf(heartbeat, sizeof(heartbeat), "%s/heartbeat", workspace);
    snprintf(done, sizeof(done), "%s/done", workspace);
    int outside_fd = open(outside, O_RDONLY);
    const char *verdict = outside_fd < 0 && errno == EPERM ? "denied" : "NOT-DENIED";
    if (outside_fd >= 0) close(outside_fd);
    int fd = open(ready, O_WRONLY | O_CREAT, 0600);
    if (fd < 0) return 1;
    write(fd, verdict, strlen(verdict));
    close(fd);
    while (!access(lease, F_OK)) {
        fd = open(heartbeat, O_WRONLY | O_CREAT | O_APPEND, 0600);
        if (fd < 0) return 1;
        write(fd, "x", 1);
        close(fd);
        usleep(20000);
    }
    fd = open(done, O_WRONLY | O_CREAT, 0600);
    if (fd >= 0) close(fd);
    return 0;
}

int main(int argc, char **argv) {
    if (argc < 3) return 64;
    int result = -1;
    if (!strcmp(argv[1], "read") || !strcmp(argv[1], "write")) {
        int writing = !strcmp(argv[1], "write");
        int fd = open(argv[2], writing ? O_WRONLY | O_CREAT | O_APPEND : O_RDONLY, 0600);
        if (fd >= 0) {
            char value = 'x';
            result = writing ? (int)write(fd, &value, 1) : (int)read(fd, &value, 1);
            close(fd);
        }
    } else if (!strcmp(argv[1], "stat")) {
        struct stat info;
        result = stat(argv[2], &info);
    } else if (!strcmp(argv[1], "chmod")) {
        result = chmod(argv[2], 0600);
    } else if (!strcmp(argv[1], "link") && argc == 4) {
        result = link(argv[2], argv[3]);
    } else if (!strcmp(argv[1], "rename-write") && argc == 5) {
        result = rename(argv[2], argv[3]);
        if (!result) {
            int fd = open(argv[4], O_WRONLY | O_APPEND);
            result = fd < 0 ? -1 : (int)write(fd, "x", 1);
            if (fd >= 0) close(fd);
        }
    } else if (!strcmp(argv[1], "tcp4") || !strcmp(argv[1], "tcp6") ||
               !strcmp(argv[1], "udp4") || !strcmp(argv[1], "udp6") || !strcmp(argv[1], "unix")) {
        result = network_probe(argv[1], argv[2]);
    } else if (!strcmp(argv[1], "victim")) {
        // Only this synthetic process is targeted by inspection/signaling probes.
        signal(SIGUSR1, SIG_IGN);
        alarm(10);
        puts("ready");
        fflush(stdout);
        return getchar() == EOF ? 0 : 1;
    } else if (!strcmp(argv[1], "pidinfo")) {
        struct proc_bsdinfo info;
        result = proc_pidinfo(atoi(argv[2]), PROC_PIDTBSDINFO, 0, &info, sizeof(info));
        if (!result) result = -1;
    } else if (!strcmp(argv[1], "procargs")) {
        int mib[] = {CTL_KERN, KERN_PROCARGS2, atoi(argv[2])};
        char buffer[65536];
        size_t size = sizeof(buffer);
        result = sysctl(mib, 3, buffer, &size, NULL, 0);
        if (!result && argc == 4) {
            size_t n = strlen(argv[3]);
            int found = 0;
            for (size_t i = 0; i + n <= size; i++) {
                if (!memcmp(buffer + i, argv[3], n)) found = 1;
            }
            if (!found) { puts("unavailable: synthetic environment marker absent"); return 2; }
        }
    } else if (!strcmp(argv[1], "signal")) {
        result = kill(atoi(argv[2]), SIGUSR1);
    } else if (!strcmp(argv[1], "taskport")) {
        mach_port_t task;
        kern_return_t kr = task_for_pid(mach_task_self(), atoi(argv[2]), &task);
        if (kr == KERN_SUCCESS) {
            mach_port_deallocate(mach_task_self(), task);
            result = 0;
        } else {
            printf("unavailable task_for_pid=%d\n", kr);
            return 2;
        }
    } else if (!strcmp(argv[1], "fdread")) {
        char value;
        result = (int)read(atoi(argv[2]), &value, 1);
    } else if (!strcmp(argv[1], "loader-status")) {
        puts(getenv("DYLD_INSERT_LIBRARIES") ? "retained" : "stripped");
        return 0;
    } else if (!strcmp(argv[1], "wait-read")) {
        puts("ready");
        fflush(stdout);
        if (getchar() == EOF) return 2;
        int fd = open(argv[2], O_RDONLY);
        if (fd >= 0) { result = 0; close(fd); }
    } else if (!strcmp(argv[1], "darwin-temp")) {
        char path[4096];
        if (!confstr(_CS_DARWIN_USER_TEMP_DIR, path, sizeof(path))) return 2;
        printf("matches-TMPDIR=%s\n", getenv("TMPDIR") && !strcmp(path, getenv("TMPDIR")) ? "yes" : "no");
        return 0;
    } else if (!strcmp(argv[1], "worker") && argc == 4) {
        return worker(argv[2], argv[3]);
    } else if (!strcmp(argv[1], "spawn") && argc == 6) {
        alarm(5);
        pid_t pid = fork();
        if (pid < 0) return 2;
        if (!pid) {
            if (!strcmp(argv[4], "detached")) {
                if (setsid() < 0) _exit(2);
                pid = fork();
                if (pid < 0) _exit(2);
                if (pid) _exit(0);
            }
            execl(argv[0], argv[0], "worker", argv[2], argv[3], (char *)NULL);
            _exit(2);
        }
        char ready[4096];
        snprintf(ready, sizeof(ready), "%s/ready", argv[3]);
        while (access(ready, F_OK)) usleep(1000);
        if (!strcmp(argv[4], "detached")) waitpid(pid, NULL, 0);
        puts("ready");
        fflush(stdout);
        if (!strcmp(argv[5], "stay")) for (;;) pause();
        return 0;
    } else {
        return 64;
    }
    if (result < 0) {
        printf("denied errno=%d (%s)\n", errno, strerror(errno));
        return 1;
    }
    puts("allowed");
    return 0;
}
