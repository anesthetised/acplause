//go:build unix

package tools

import "syscall"

// nonblock prevents opening a FIFO from waiting before we can reject it.
const nonblock = syscall.O_NONBLOCK
