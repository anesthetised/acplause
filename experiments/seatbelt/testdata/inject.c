// Test-only loader canary. Its only target is a disposable outside fixture.
#include <fcntl.h>
#include <stdlib.h>
#include <unistd.h>

__attribute__((constructor)) static void canary(void) {
    const char *path = getenv("FIXTURE_CANARY");
    if (!path) return;
    int fd = open(path, O_WRONLY | O_CREAT | O_APPEND, 0600);
    if (fd >= 0) {
        write(fd, "x", 1);
        close(fd);
    }
}
