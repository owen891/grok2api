//go:build windows

package registration

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func prepareProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	output, err := exec.Command("tasklist.exe", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	return err == nil && strings.Contains(string(output), `"`+strconv.Itoa(pid)+`"`)
}

func isRegistrationProcess(pid int) bool {
	if !processExists(pid) {
		return false
	}
	command := fmt.Sprintf("(Get-CimInstance Win32_Process -Filter 'ProcessId=%d').CommandLine", pid)
	output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command).Output()
	if err != nil {
		return false
	}
	line := strings.ToLower(string(output))
	return strings.Contains(line, "register_cli.py") || strings.Contains(line, "grok2api-registration") || strings.Contains(line, "testregistrationhelperprocess")
}

func stopProcessTree(ctx context.Context, pid int) error {
	if !isRegistrationProcess(pid) {
		return nil
	}
	killTimeout := 3 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx.Err()
		}
		if remaining < killTimeout {
			killTimeout = remaining
		}
	}
	killCtx, cancel := context.WithTimeout(context.Background(), killTimeout)
	output, err := exec.CommandContext(killCtx, "taskkill.exe", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
	cancel()
	if err != nil && processExists(pid) {
		// A busy child process can make /T hang. Fall back to terminating the
		// worker itself so the controller never reports success while it lives.
		directCtx, directCancel := context.WithTimeout(context.Background(), 2*time.Second)
		directOutput, directErr := exec.CommandContext(directCtx, "taskkill.exe", "/PID", strconv.Itoa(pid), "/F").CombinedOutput()
		directCancel()
		if directErr != nil && processExists(pid) {
			message := strings.TrimSpace(string(directOutput))
			if message == "" {
				message = strings.TrimSpace(string(output))
			}
			return fmt.Errorf("taskkill: %w: %s", directErr, message)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if processExists(pid) {
		return errors.New("注册进程树未退出")
	}
	return nil
}
