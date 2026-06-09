// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package main

import (
	"os/exec"
	"syscall"
)

// setProcAttr configures a worker process for supervision on non-Linux
// platforms (e.g. macOS).
//
// Setpgid puts the worker in its own process group so we can signal the whole
// tree on shutdown. These platforms lack Linux's Pdeathsig, so a supervisor
// crash can briefly orphan a worker; on shutdown we still SIGKILL the process
// group, and Setpgid keeps that working.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
