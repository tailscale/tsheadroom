// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setProcAttr configures a worker process for supervision on Linux.
//
// Pdeathsig: the kernel SIGKILLs the worker if we (the parent) die, so no
// orphans survive a supervisor crash. Setpgid: own process group, so we can
// signal the whole tree on shutdown.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}
