package main

import (
	"os/exec"
)

// ProcessManager 跨平台进程管理接口
type ProcessManager interface {
	SetupDaemonProcess(cmd *exec.Cmd)
	IsProcessRunning(pid int) bool
	IsCloudflaredProcess(pid int) bool
	KillProcess(pid int, force bool) error
	KillProcessGroup(pid int, force bool) error
	FindProcessByName(name string) []int
	ResetTerminal()
}

// 前向声明具体实现类型（由 process_unix.go 和 process_windows.go 提供）
type unixProcessManager struct{}
type windowsProcessManager struct{}
