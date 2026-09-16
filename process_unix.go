//go:build !windows
// +build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

func (u *unixProcessManager) SetupDaemonProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}
}

func (u *unixProcessManager) IsProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

// IsCloudflaredProcess 校验指定 PID 确实属于 cloudflared 进程
// 防止 PID 复用后误杀无关进程：先查 /proc/<pid>/exe，再回退 cmdline
func (u *unixProcessManager) IsCloudflaredProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		if strings.Contains(strings.ToLower(filepath.Base(exe)), "cloudflared") {
			return true
		}
	}
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "cloudflared") {
			return true
		}
	}
	return false
}

func (u *unixProcessManager) KillProcess(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(pid, sig)
}

func (u *unixProcessManager) KillProcessGroup(pid int, force bool) error {
	processGroupID, err := syscall.Getpgid(pid)
	if err == nil && processGroupID > 0 {
		u.KillProcess(-processGroupID, force)
	}
	return u.KillProcess(pid, force)
}

func (u *unixProcessManager) FindProcessByName(name string) []int {
	var pids []int

	cmd := exec.Command("pgrep", "-f", "cloudflared.*"+name)
	output, _ := cmd.Output()
	for _, line := range strings.Split(string(output), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			pids = append(pids, pid)
		}
	}

	if len(pids) == 0 {
		// [安全] 单引号包裹并转义，防止隧道名注入 shell
		safeName := strings.ReplaceAll(name, "'", `'\''`)
		cmd = exec.Command("sh", "-c",
			fmt.Sprintf("ps aux | grep -v grep | grep 'cloudflared.*%s' | awk '{print $2}'", safeName))
		output, _ = cmd.Output()
		for _, line := range strings.Split(string(output), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
				pids = append(pids, pid)
			}
		}
	}

	return pids
}

func (u *unixProcessManager) ResetTerminal() {
	fmt.Print("[0m[?25h[?7h[H")
	os.Stdout.Sync()

	if term.IsTerminal(int(os.Stdin.Fd())) {
		if err := exec.Command("stty", "sane").Run(); err != nil {
			exec.Command("stty", "-raw", "echo", "icanon", "icrnl").Run()
		}
	}
	time.Sleep(50 * time.Millisecond)
}

// NewProcessManager 创建平台适配的进程管理器
func NewProcessManager() ProcessManager {
	return &unixProcessManager{}
}
