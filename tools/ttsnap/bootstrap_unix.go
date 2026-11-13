// Copyright (C) 2019-2026 Algorand Foundation Ltd.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// phaseOwner returns the user to run the install and catchup phases as: the
// owner of dataDir when ttsnap runs as root and another user owns it, so that
// what they write stays the node's and root writes nothing, and reads
// nothing, in a directory that user controls. nil: run them in this process.
func phaseOwner(dataDir string) (*phaseUser, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	fi, err := os.Stat(dataDir)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil, nil
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil {
		return nil, fmt.Errorf("%s belongs to user %d: %w", dataDir, st.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %s's group %s: %w", u.Username, u.Gid, err)
	}
	pu := &phaseUser{name: u.Username, home: u.HomeDir, uid: st.Uid, gid: uint32(gid)}
	gids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("user %s's groups: %w", u.Username, err)
	}
	for _, g := range gids {
		n, perr := strconv.ParseUint(g, 10, 32)
		if perr != nil {
			return nil, fmt.Errorf("user %s's group %s: %w", u.Username, g, perr)
		}
		pu.groups = append(pu.groups, uint32(n))
	}
	return pu, nil
}

// runPhase runs ttsnap bootstrap's phase as u, with args, and reports
// whether the install phase found algod running.
func runPhase(u *phaseUser, phase string, args []string) (running bool, err error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	fmt.Printf("running the %s as %s\n", phase, u.name)
	cmd := exec.Command(exe, append([]string{"bootstrap"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), bootstrapPhaseEnv+"="+phase)
	err = runAs(cmd, u)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return false, nil
	case phase == "install" && errors.As(err, &ee) && ee.ExitCode() == exitNodeRunning:
		return true, nil
	case errors.As(err, &ee):
		// the child reported its error
		return false, fmt.Errorf("the %s as %s failed", phase, u.name)
	}
	return false, fmt.Errorf("running the %s as %s: %w", phase, u.name, err)
}

// runAs runs cmd as u, in a session of its own and without stdin: a process
// of u's can't reach root's terminal through it. The signals that stop
// bootstrap are passed on, as the terminal no longer sends them.
func runAs(cmd *exec.Cmd, u *phaseUser) error {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	env := cmd.Env[:0:0]
	for _, kv := range cmd.Env {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "HOME", "USER", "LOGNAME":
		default:
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "HOME="+u.home, "USER="+u.name, "LOGNAME="+u.name)
	cmd.Dir = "/"
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: u.groups}}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-sigs:
				// the whole group: Setsid made the child its leader, and a
				// start command's shell has children of its own
				_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
			case <-done:
				return
			}
		}
	}()
	return cmd.Wait()
}

func shellCommand(command string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", command)
}
