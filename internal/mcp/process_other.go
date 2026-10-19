//go:build !unix

package mcp

import "os/exec"

func configureProcess(*exec.Cmd)          {}
func signalProcess(cmd *exec.Cmd, _ bool) { _ = cmd.Process.Kill() }
