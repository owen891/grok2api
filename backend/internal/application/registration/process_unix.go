//go:build !windows

package registration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func prepareProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}

func isRegistrationProcess(pid int) bool {
	if !processExists(pid) {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	command := strings.ReplaceAll(string(data), "\x00", " ")
	return strings.Contains(command, "register_cli.py") || strings.Contains(command, "grok2api-registration") || strings.Contains(command, "TestRegistrationHelperProcess")
}

func stopProcessTree(ctx context.Context, pid int) error {
	if !isRegistrationProcess(pid) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitProcessExit(ctx, pid, 10*time.Second) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if waitProcessExit(ctx, pid, 5*time.Second) {
		return nil
	}
	return errors.New("注册进程树未退出")
}

func waitProcessExit(ctx context.Context, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processExists(pid) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return !processExists(pid)
}
