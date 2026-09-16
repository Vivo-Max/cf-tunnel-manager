//go:build windows
// +build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func (w *windowsProcessManager) SetupDaemonProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func (w *windowsProcessManager) IsProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	err = windows.GetExitCodeProcess(handle, &exitCode)
	// STILL_ACTIVE = 259
	return err == nil && exitCode == 259
}

// IsCloudflaredProcess 校验指定 PID 的可执行文件名确实属于 cloudflared
// 防止 PID 复用后误杀无关进程
func (w *windowsProcessManager) IsCloudflaredProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var buf [32768]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(windows.UTF16ToString(buf[:size])), "cloudflared")
}

func (w *windowsProcessManager) KillProcess(pid int, force bool) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}

	var cmd *exec.Cmd
	if force {
		cmd = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
	} else {
		cmd = exec.Command("taskkill", "/PID", strconv.Itoa(pid))
	}
	return cmd.Run()
}

func (w *windowsProcessManager) KillProcessGroup(pid int, force bool) error {
	return w.KillProcess(pid, force)
}

func (w *windowsProcessManager) FindProcessByName(name string) []int {
	var pids []int

	// [修复] 优先用 wmic 获取命令行，按隧道名精确匹配，
	// 避免旧的 tasklist 方式无法区分隧道、误杀所有 cloudflared 进程
	cmd := exec.Command("wmic", "process", "where", "name='cloudflared.exe'", "get", "commandline,processid")
	output, err := cmd.Output()
	if err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(strings.ToLower(line), "commandline") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			pid, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				continue
			}
			// 命令行包含隧道名才算匹配（--config <name>.yml / run <name>）
			if strings.Contains(line, name) {
				pids = append(pids, pid)
			}
		}
		if len(pids) > 0 {
			return pids
		}
	}

	// 回退：tasklist 只能按镜像名匹配，无法区分隧道（可能匹配到全部隧道进程）
	cmd = exec.Command("tasklist", "/FI", "IMAGENAME eq cloudflared.exe", "/FO", "CSV", "/NH")
	output, err = cmd.Output()
	if err != nil {
		return pids
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Split(line, "\",\"")
		if len(fields) >= 2 {
			pidStr := strings.Trim(fields[1], "\"\"")
			if pid, err := strconv.Atoi(pidStr); err == nil {
				pids = append(pids, pid)
			}
		}
	}

	return pids
}

func (w *windowsProcessManager) ResetTerminal() {
	fmt.Print("\033[0m\033[?25h\033[?7h\033[H")
	os.Stdout.Sync()
	time.Sleep(50 * time.Millisecond)
}

// NewProcessManager 创建平台适配的进程管理器
func NewProcessManager() ProcessManager {
	return &windowsProcessManager{}
}
