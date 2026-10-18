//go:build !unix

package tools

import "os/exec"

// killProcessGroupOnCancel keeps exec's default of killing only the shell.
func killProcessGroupOnCancel(*exec.Cmd) {}
