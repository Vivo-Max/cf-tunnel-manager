package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/eiannone/keyboard"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// ====== 颜色常量（VPS 优化：纯 ASCII）======
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorPurple = "\033[35m"
	ColorCyan   = "\033[36m"
	ColorWhite  = "\033[37m"
)

// 全局版本号定义
const Version = "v3.5"

// randomColor 返回随机 ANSI 颜色代码，用于标题框边框渲染
func randomColor() string {
	colors := []string{
		ColorRed, ColorGreen, ColorYellow, ColorBlue, ColorPurple, ColorCyan,
	}
	return colors[rand.Intn(len(colors))]
}

// getStringDisplayWidth 计算字符串在终端中的显示宽度
// 中文字符和 Emoji 计为 2 宽度，ASCII 字符计为 1 宽度
// 用于文本对齐计算，确保菜单在终端中整齐排列
func getStringDisplayWidth(s string) int {
	width := 0
	for _, r := range s {
		if utf8.RuneLen(r) > 1 {
			width += 2
		} else {
			width += 1
		}
	}
	return width
}

// removeColorCodes 移除字符串中的 ANSI 颜色转义序列
// 用于在计算显示宽度前清理颜色代码，确保对齐准确
func removeColorCodes(p []byte) []byte {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAll(p, []byte(""))
}

// drawCenteredTitleBox 绘制居中的 ASCII 标题框
// 参数 title: 标题文本（支持颜色代码），width: 终端宽度
// 根据内容自动计算内边距，确保边框对齐，过宽时自动左对齐
func drawCenteredTitleBox(title string, width int) {
	borderColor := randomColor()

	cleanTitle := removeColorCodes([]byte(title))
	titleRunes := []rune(string(cleanTitle))
	titleDisplayWidth := 0
	for _, r := range titleRunes {
		if r > 0x1F300 || (r >= 0x2600 && r <= 0x27BF) {
			titleDisplayWidth += 2
		} else if utf8.RuneLen(r) > 1 {
			titleDisplayWidth += 2
		} else {
			titleDisplayWidth += 1
		}
	}

	innerBoxWidth := titleDisplayWidth + 4
	boxTotalWidth := innerBoxWidth + 2

	if width < boxTotalWidth {
		width = boxTotalWidth + 10
	}

	if boxTotalWidth >= width {
		fmt.Println()
		fmt.Println(borderColor + "+" + strings.Repeat("-", innerBoxWidth) + "+" + ColorReset)
		fmt.Println(borderColor + "|  " + ColorReset + title + borderColor + "  |" + ColorReset)
		fmt.Println(borderColor + "+" + strings.Repeat("-", innerBoxWidth) + "+" + ColorReset)
		fmt.Println()
		return
	}

	padding := (width - boxTotalWidth) / 2
	paddingStr := strings.Repeat(" ", padding)

	topBorder := paddingStr + borderColor + "+" + strings.Repeat("-", innerBoxWidth) + "+" + ColorReset
	titleLine := paddingStr + borderColor + "|  " + ColorReset + title + borderColor + "  |" + ColorReset
	bottomBorder := paddingStr + borderColor + "+" + strings.Repeat("-", innerBoxWidth) + "+" + ColorReset

	fmt.Println()
	fmt.Println(topBorder)
	fmt.Println(titleLine)
	fmt.Println(bottomBorder)
	fmt.Println()
}

// 配置目录常量定义
const (
	configDir = ".cloudflared"              // Cloudflared 配置目录
	binDir    = ".local/bin"                // 二进制文件安装目录
	saveFile  = ".cloudflared-tunnels.yaml" // 隧道保存文件
	backupDir = ".cloudflared-backups"      // 备份文件存放目录
)

// 动态设置临时目录（VPS 优化：使用 home 目录避免 /tmp 被清理）
var (
	logDir = getLogDir() // 日志文件存放路径
	pidDir = getPIDDir() // PID 文件存放路径
)

// resetKeyboard 强制重置键盘监听状态
// 修复：增加错误处理和重试逻辑
func resetKeyboard() {
	keyboard.Close()
	time.Sleep(150 * time.Millisecond)

	// 最多重试 3 次
	for i := 0; i < 3; i++ {
		if err := keyboard.Open(); err == nil {
			return // 成功
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 最终失败也不 panic，让调用者处理
}

// getLogDir 获取日志存放目录
// VPS 优化：统一使用用户主目录下的 .cloudflared-logs，避免 /tmp 被系统自动清理导致日志丢失
func getLogDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cloudflared-logs")
	os.MkdirAll(dir, 0755)
	return dir
}

// getPIDDir 获取 PID 文件存放目录
// VPS 优化：统一使用用户主目录下的 .cloudflared-pids，确保进程管理可靠
func getPIDDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cloudflared-pids")
	os.MkdirAll(dir, 0755)
	return dir
}

// PlatformInfo 存储平台相关信息
type PlatformInfo struct {
	OS          string // 操作系统 (linux/darwin/windows)
	Arch        string // 架构 (amd64/arm64等)
	IsDocker    bool   // 是否在 Docker 环境中运行
	DownloadURL string // Cloudflared 下载链接
	BinaryName  string // 二进制文件名 (cloudflared 或 cloudflared.exe)
}

// getPlatformInfo 检测当前运行平台并返回下载信息
// 自动识别操作系统、架构，生成对应的 Cloudflared 下载链接
// 支持 Docker 环境检测
func getPlatformInfo() PlatformInfo {
	info := PlatformInfo{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	info.IsDocker = isRunningInDocker()

	// 允许用户通过环境变量强制覆盖（应急方案）
	if forcedArch := os.Getenv("CF_ARCH"); forcedArch != "" {
		info.Arch = forcedArch
		fmt.Printf("%s[!] 使用环境变量覆盖架构: %s%s\n", ColorYellow, forcedArch, ColorReset)
	}

	// Android 二次确认：若内核报告 aarch64 但 Go 说 amd64，强制修正
	if info.OS == "android" && info.Arch == "amd64" {
		if cmdOut, err := exec.Command("uname", "-m").Output(); err == nil {
			arch := strings.TrimSpace(string(cmdOut))
			switch arch {
			case "aarch64":
				info.Arch = "arm64"
				fmt.Printf("%s[!] 架构修正: amd64 -> arm64 (内核报告 aarch64)%s\n", ColorYellow, ColorReset)
			case "armv7l", "armv8l":
				info.Arch = "arm"
				fmt.Printf("%s[!] 架构修正: amd64 -> arm (内核报告 %s)%s\n", ColorYellow, arch, ColorReset)
			}
		}
	}

	archMap := map[string]string{
		"amd64": "amd64",
		"386":   "386",
		"arm":   "arm",
		"arm64": "arm64",
	}

	cloudflaredArch, ok := archMap[info.Arch]
	if !ok {
		cloudflaredArch = "amd64"
	}

	baseURL := "https://github.com/cloudflare/cloudflared/releases/latest/download"

	if info.IsDocker {
		info.BinaryName = "cloudflared"
		info.DownloadURL = fmt.Sprintf("%s/cloudflared-linux-%s", baseURL, cloudflaredArch)
	} else {
		switch info.OS {
		case "linux", "android": // ← Android 内核就是 Linux，使用相同二进制
			info.BinaryName = "cloudflared"
			info.DownloadURL = fmt.Sprintf("%s/cloudflared-linux-%s", baseURL, cloudflaredArch)
		case "darwin":
			info.BinaryName = "cloudflared"
			if info.Arch == "arm64" {
				info.DownloadURL = fmt.Sprintf("%s/cloudflared-darwin-arm64.tgz", baseURL)
			} else {
				info.DownloadURL = fmt.Sprintf("%s/cloudflared-darwin-amd64.tgz", baseURL)
			}
		case "windows":
			info.BinaryName = "cloudflared.exe"
			info.DownloadURL = fmt.Sprintf("%s/cloudflared-windows-%s.exe", baseURL, cloudflaredArch)
		default:
			info.BinaryName = "cloudflared"
			info.DownloadURL = fmt.Sprintf("%s/cloudflared-linux-%s", baseURL, cloudflaredArch)
		}
	}

	return info
}

// isRunningInDocker 检测当前是否在 Docker 容器中运行
// 通过检查 /.dockerenv 文件和 /proc/1/cgroup 内容判断
func isRunningInDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err == nil && strings.Contains(string(data), "docker") {
		return true
	}
	return false
}

// String 返回平台信息的字符串表示
func (p PlatformInfo) String() string {
	dockerTag := ""
	if p.IsDocker {
		dockerTag = "[Docker] "
	}
	return fmt.Sprintf("%s%s/%s", dockerTag, p.OS, p.Arch)
}

// Tunnel 存储单个隧道的配置信息
type Tunnel struct {
	Name          string `yaml:"name"`                    // 隧道名称
	Domain        string `yaml:"domain"`                  // 根域名 (如 example.com)
	Subdomain     string `yaml:"subdomain"`               // 子域名前缀 (如 api)
	Port          string `yaml:"port"`                    // 本地服务端口
	ServiceScheme string `yaml:"service_scheme,omitempty"` // 本地服务协议：空/http 兼容旧配置；ssh 表示 SSH 穿透
	TunnelID      string `yaml:"tunnel_id"`               // Cloudflare 分配的隧道 UUID
	Active        bool   `yaml:"active"`                  // 是否为默认隧道
}

// APICredentials 存储 Cloudflare API Token 凭证（无浏览器环境使用）
type APICredentials struct {
	Token     string `yaml:"api_token"`
	AccountID string `yaml:"account_id"`
}

// Config 存储所有隧道配置
type Config struct {
	Tunnels        []Tunnel        `yaml:"tunnels"`
	APICredentials *APICredentials `yaml:"api_credentials,omitempty"`
}

// 全局平台信息变量
var platformInfo PlatformInfo
var homeDir string
var procMgr = NewProcessManager()

// createNewTunnel 的一次性预置参数：仅用于“本机SSH穿透”入口，使用后立即清空。
// 不改变原交互式新建隧道的默认值：未设置时仍等同于旧版 http + 8080 + mytunnel。
var presetServiceScheme string
var presetPort string
var presetName string

// configLoadFailed 标记配置文件解析失败：本次运行拒绝写回，防止空配置覆盖原文件
var configLoadFailed bool

// clearScreen 彻底清屏（仅使用 ANSI 序列，避免执行命令产生空行）
func clearScreen() {
	// 清屏 + 清除滚动缓冲区 + 光标归位（三合一 ANSI 序列）
	fmt.Print("\033[2J\033[3J\033[H")

}

// showTraditionalMenu 显示传统交互式菜单（VPS 优化版）
// 支持键盘方向键导航和数字输入两种模式
// 自动检测终端是否支持键盘事件，不支持时降级为数字输入模式
// 参数 cfg: 当前配置，homeDir: 用户主目录，useKeyboard: 是否使用键盘事件，reader: 标准输入读取器
func showTraditionalMenu(cfg Config, homeDir string, useKeyboard bool, reader *bufio.Reader) {
	rand.Seed(time.Now().UnixNano())

	binPath := filepath.Join(homeDir, binDir)
	configPath := filepath.Join(homeDir, configDir)

	if useKeyboard {
		if err := keyboard.Open(); err != nil {
			useKeyboard = false
		} else {
			defer keyboard.Close()
		}
	}

	selected := 1

	// VPS 优化：使用 ASCII 符号替代 Emoji，确保对齐
	options := []struct {
		index int
		icon  string
		text  string
		desc  string
	}{
		{1, ">", "快速启动", "使用上次的配置(静默模式)"},
		{2, "+", "新建隧道", "创建新的域名穿透"},
		{3, "#", "管理隧道", "查看/编辑/删除/后台管理"},
		{4, "~", "切换隧道", "选择并启动其他隧道"},
		{5, "@", "导入导出", "备份或恢复配置"},
		{6, "*", "证书管理", "管理 HTTPS 证书"},
		{7, ":", "后台管理", "查看/终止后台隧道"},
		{8, "-", "删除配置", "清除所有本地配置"},
		{9, "X", "退出程序", "关闭管理器"},
		{10, "D", "重新下载/修复", "重新下载或修复 cloudflared 二进制"},
		{11, "$", "本机SSH穿透", "一键创建 ssh://localhost:22 隧道"},
	}

	maxTextWidth := 0
	for _, opt := range options {
		w := getStringDisplayWidth(opt.text)
		if w > maxTextWidth {
			maxTextWidth = w
		}
	}

	for {
		// [修复 1] 每次循环强制重新检测终端宽度（防止子进程或窗口 resize 后失效）
		width := 80
		if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
			if w, _, err := term.GetSize(fd); err == nil && w > 0 {
				width = w
			}
		}

		// [修复 2] 在清屏前短暂延迟，确保终端准备好
		time.Sleep(50 * time.Millisecond)
		clearScreen()
		drawCenteredTitleBox(ColorYellow+"[ Cloudflare 隧道管理器 "+Version+" ]"+ColorReset, width)

		fmt.Printf("%s平台: %s%s\n", ColorBlue, platformInfo.String(), ColorReset)
		if !useKeyboard {
			fmt.Printf("%s模式: %s数字输入（输入数字后回车）%s\n", ColorBlue, ColorYellow, ColorReset)
		}

		// [修改] 显示运行中隧道真实状态（验证连接而不仅是进程）
		connectedCount := 0
		startingCount := 0
		errorCount := 0

		for _, t := range cfg.Tunnels {
			status := checkTunnelRealStatus(t.Name)

			if !status.Running {
				continue
			}

			protoInfo := ""
			if status.Protocol != "" {
				protoInfo = fmt.Sprintf(" 协议:%s", status.Protocol)
			}

			if status.Connected {
				connectedCount++
				fmt.Printf("%s[已连接] %s PID:%d%s 日志:%s%s\n",
					ColorGreen, t.Name, getDaemonPID(t.Name), protoInfo,
					getLogPath(t.Name), ColorReset)
			} else if status.LastError != "" {
				errorCount++
				fmt.Printf("%s[错误] %s PID:%d 错误:%s%s 日志:%s%s\n",
					ColorRed, t.Name, getDaemonPID(t.Name), status.LastError,
					protoInfo, getLogPath(t.Name), ColorReset)
			} else {
				startingCount++
				fmt.Printf("%s[启动中] %s PID:%d%s 日志:%s%s\n",
					ColorYellow, t.Name, getDaemonPID(t.Name), protoInfo,
					getLogPath(t.Name), ColorReset)
			}
		}

		// 显示汇总状态
		totalActive := connectedCount + startingCount + errorCount
		if totalActive > 0 {
			fmt.Printf("%s当前状态: ", ColorBlue)
			if connectedCount > 0 {
				fmt.Printf("%s%d 个已连接%s ", ColorGreen, connectedCount, ColorReset)
			}
			if startingCount > 0 {
				fmt.Printf("%s%d 个启动中%s ", ColorYellow, startingCount, ColorReset)
			}
			if errorCount > 0 {
				fmt.Printf("%s%d 个错误%s ", ColorRed, errorCount, ColorReset)
			}
			fmt.Println()
		} else {
			fmt.Printf("%s当前状态: %s无运行中隧道%s\n", ColorBlue, ColorYellow, ColorReset)
		}
		fmt.Println()

		fmt.Println(ColorYellow + "--- 请选择一个操作 ---" + ColorReset)

		for _, opt := range options {
			prefix := "   "
			lineColor := ColorReset
			if opt.index == selected {
				prefix = " > "
				lineColor = ColorCyan
			}

			currentWidth := getStringDisplayWidth(opt.text)
			padCount := maxTextWidth - currentWidth
			if padCount < 0 {
				padCount = 0
			}
			padding := strings.Repeat(" ", padCount)

			fmt.Printf("%s%s%2d. [%s] %s%s - %s%s\n",
				prefix, lineColor, opt.index, opt.icon, opt.text, padding, opt.desc, ColorReset)
		}

		isExecuted := false

		if !useKeyboard {
			fmt.Printf("\n%s输入数字(1-11)后回车，Q退出: %s", ColorGreen, ColorReset)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if input == "q" {
				fmt.Println(ColorRed + "\n[ 再见！ ]" + ColorReset)
				return
			}
			if num, err := strconv.Atoi(input); err == nil && num >= 1 && num <= len(options) {
				selected = num
				isExecuted = true
			}
		} else {
			fmt.Printf("\n%s   [↑↓] 选择   [ESC/Q] 退出   [Enter] 执行%s: ",
				ColorGreen, ColorReset)

			char, key, err := keyboard.GetKey()
			if err != nil {
				useKeyboard = false
				continue
			}

			switch key {
			case keyboard.KeyArrowUp:
				selected--
				if selected < 1 {
					selected = len(options)
				}
			case keyboard.KeyArrowDown:
				selected++
				if selected > len(options) {
					selected = 1
				}
			case keyboard.KeyEnter:
				isExecuted = true
			case keyboard.KeyEsc:
				fmt.Println(ColorRed + "\n[ 再见！ ]" + ColorReset)
				return
			default:
				if char >= '1' && char <= '9' {
					selected = int(char - '0')
					isExecuted = true
				} else if char == '0' {
					selected = 10 // 选项 10 用键盘输入 0 选择
					isExecuted = true
				} else if char == 'Q' || char == 'q' {
					fmt.Println(ColorRed + "\n[ 再见！ ]" + ColorReset)
					return
				} else if char == 's' {
					selected = 11
				} else if char == 'S' {
					selected = 11
					isExecuted = true
				}
			}
		}

		if isExecuted {
			clearScreen()
			drawCenteredTitleBox(ColorYellow+"[ Cloudflare 隧道管理器 "+Version+" ]"+ColorReset, width)

			switch selected {
			case 1:
				if len(cfg.Tunnels) == 0 {
					fmt.Println(errorStyle.Render("[!] 没有可用配置，请先创建隧道"))
					time.Sleep(2 * time.Second)
					continue
				}

				var target Tunnel
				for _, t := range cfg.Tunnels {
					if t.Active {
						target = t
						break
					}
				}
				if target.Name == "" {
					target = cfg.Tunnels[0]
				}

				if useKeyboard {
					keyboard.Close()
					time.Sleep(100 * time.Millisecond)
				}

				// [关键修改] 使用静默模式启动，日志进文件，终端保持清爽
				_ = runTunnelSilent(target, binPath, useKeyboard, reader)

				// 重置终端状态
				resetTerminal()
				time.Sleep(200 * time.Millisecond)

				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(300 * time.Millisecond)
					}
				}

			case 2:
				if useKeyboard {
					keyboard.Close()
				}
				createNewTunnel(&cfg, configPath, binPath, useKeyboard, reader)
				saveConfig(cfg, filepath.Join(homeDir, saveFile))
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
				time.Sleep(100 * time.Millisecond)

			case 3:
				if useKeyboard {
					keyboard.Close()
				}
				manageTunnels(&cfg, configPath, binPath, useKeyboard, reader)
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
				saveConfig(cfg, filepath.Join(homeDir, saveFile))

			case 4:
				if useKeyboard {
					keyboard.Close()
				}
				switchTunnel(&cfg, configPath, binPath, useKeyboard, reader)
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
				saveConfig(cfg, filepath.Join(homeDir, saveFile))

			case 5:
				if useKeyboard {
					keyboard.Close()
				}
				backupRestoreMenu(&cfg, homeDir, configPath, useKeyboard, reader)
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
				saveConfig(cfg, filepath.Join(homeDir, saveFile))

			case 6:
				if useKeyboard {
					keyboard.Close()
				}
				certManager(configPath, binPath, useKeyboard, reader)
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}

			case 7:
				if useKeyboard {
					keyboard.Close()
				}
				daemonManager(cfg, binPath, useKeyboard, reader)
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}

			case 8:
				if useKeyboard {
					keyboard.Close()
				}
				cleanConfig(&cfg, homeDir, configPath, useKeyboard, reader)
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
				saveConfig(cfg, filepath.Join(homeDir, saveFile))

			case 9:
				fmt.Println(ColorRed + "\n[ 再见！ ]" + ColorReset)
				return

			case 11:
				if useKeyboard {
					keyboard.Close()
				}
				startLocalSSHTunnelPreset(&cfg, configPath, binPath, useKeyboard, reader)
				saveConfig(cfg, filepath.Join(homeDir, saveFile))
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}

			case 10:
				// [新增] 手动重新下载/修复 cloudflared：清理失败标记并强制重下
				if useKeyboard {
					keyboard.Close()
				}
				fmt.Print(warningStyle.Render("[!] 将删除现有 cloudflared 并重新下载，确定? [yes/N]: "))
				confirm, _ := reader.ReadString('\n')
				if strings.TrimSpace(strings.ToLower(confirm)) != "yes" {
					fmt.Println(infoStyle.Render("[已取消]"))
					time.Sleep(1 * time.Second)
				} else {
					// 删除可能损坏的二进制 + 清理失败标记，然后重新走下载流程
					os.Remove(filepath.Join(binPath, platformInfo.BinaryName))
					clearDownloadFailMarks(filepath.Join(homeDir, configDir))
					ensureCloudflared(homeDir)
				}
				resetTerminal()
				if useKeyboard {
					for i := 0; i < 3; i++ {
						if err := keyboard.Open(); err == nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
		}
	}
}

// ====== 准确的隧道状态检测（新增）======

// TunnelStatus 隧道详细状态
type TunnelStatus struct {
	Running        bool      // 进程是否在运行
	Connected      bool      // 是否成功连接到 Cloudflare
	Protocol       string    // 当前使用的协议
	LastError      string    // 最后的错误信息
	ConnectionTime time.Time // 连接成功时间
	MetricsPort    int       // 指标服务器端口
}

// readLogTail 只读取日志文件尾部指定字节数（默认调用用 64KB）
// [修复] 避免日志文件长大后每次状态检查都整文件读取导致菜单卡顿
func readLogTail(logPath string, maxBytes int64) string {
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// checkTunnelRealStatus 检查隧道的真实状态
// 不仅检查进程，还验证是否真正连接到 Cloudflare 边缘
func checkTunnelRealStatus(tunnelName string) TunnelStatus {
	status := TunnelStatus{
		Running:   false,
		Connected: false,
		Protocol:  getProtocolForTunnel(tunnelName),
	}

	logPath := getLogPath(tunnelName)
	pid := getDaemonPID(tunnelName)

	// 1. 检查进程是否存在
	if pid <= 0 || !isProcessRunning(pid) {
		// 检查是否有僵尸PID文件
		if pid > 0 {
			os.Remove(getPIDPath(tunnelName))
		}
		return status
	}
	status.Running = true

	// 2. 读取日志验证连接状态（只读尾部 64KB，防止大日志拖慢菜单）
	content := readLogTail(logPath, 64<<10)
	if content == "" {
		// 文件不存在或为空时保持原行为：返回仅 Running 的状态
		if _, err := os.Stat(logPath); err != nil {
			return status
		}
	}

	// 3. 检查成功连接标志
	successPatterns := []string{
		"Registered tunnel connection",
		"INF Connected",
		"Starting metrics server",
		"Tunnel server started",
	}

	for _, pattern := range successPatterns {
		if strings.Contains(content, pattern) {
			status.Connected = true
			break
		}
	}

	// 4. 检查错误信息
	errorPatterns := map[string]string{
		"Unable to establish connection": "连接失败",
		"Authentication error":           "认证失败",
		"Invalid tunnel credentials":     "凭证无效",
		"Failed to create QUIC":          "QUIC创建失败",
		"Failed to create HTTP/2":        "HTTP/2创建失败",
		"connection refused":             "连接被拒绝",
		"i/o timeout":                    "连接超时",
		"no such host":                   "DNS解析失败",
		"context deadline exceeded":      "连接超时",
	}

	for pattern, desc := range errorPatterns {
		if strings.Contains(content, pattern) {
			status.LastError = desc
			// 如果有错误且没有成功标志，视为未连接
			if !status.Connected {
				status.Connected = false
			}
			break
		}
	}

	// 5. 检查日志时间，如果超过30秒没有新内容，可能已卡住
	if info, err := os.Stat(logPath); err == nil {
		if time.Since(info.ModTime()) > 30*time.Second && !status.Connected {
			status.LastError = "日志无更新，可能已卡住"
		}
	}

	return status
}

// getTunnelStatusDisplay 获取用于显示的隧道状态字符串
func getTunnelStatusDisplay(tunnelName string) (string, string) {
	status := checkTunnelRealStatus(tunnelName)

	if !status.Running {
		return "[STOP]", ColorRed
	}

	if status.Connected {
		return "[CONNECTED]", ColorGreen
	}

	// 正在运行但未连接
	if status.LastError != "" {
		return "[ERROR:" + status.LastError + "]", ColorRed
	}

	return "[STARTING]", ColorYellow
}

// isTunnelTrulyRunning 判断隧道是否真正运行（进程+连接）
func isTunnelTrulyRunning(tunnelName string) bool {
	status := checkTunnelRealStatus(tunnelName)
	return status.Running && status.Connected
}

// waitForTunnelConnection 等待隧道真正连接成功
// 返回是否成功，超时时间30秒
func waitForTunnelConnection(tunnelName string, timeout time.Duration) bool {
	start := time.Now()
	for time.Since(start) < timeout {
		status := checkTunnelRealStatus(tunnelName)
		if status.Connected {
			return true
		}
		if !status.Running {
			return false
		}
		if status.LastError != "" {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// ====== 智能协议选择（新增）======

// getProtocolForTunnel 获取隧道应使用的协议
// 优先使用已保存的配置，否则返回默认协议
func getProtocolForTunnel(tunnelName string) string {
	protocolCache := filepath.Join(getPIDDir(), "."+tunnelName+".protocol")
	if data, err := os.ReadFile(protocolCache); err == nil {
		protocol := strings.TrimSpace(string(data))
		if protocol == "http2" || protocol == "quic" || protocol == "auto" {
			return protocol
		}
	}
	return "quic" // 默认使用 quic，比 http2 更抗干扰
}

// saveProtocolPreference 保存协议偏好
func saveProtocolPreference(tunnelName, protocol string) {
	protocolCache := filepath.Join(getPIDDir(), "."+tunnelName+".protocol")
	os.WriteFile(protocolCache, []byte(protocol), 0644)
}

// updateConfigProtocol 更新配置文件中的协议
func updateConfigProtocol(configFile, newProtocol string) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return
	}

	content := string(data)

	// 替换 protocol: xxx 行
	re := regexp.MustCompile(`(?m)^protocol:\s*\w+`)
	if re.MatchString(content) {
		newContent := re.ReplaceAllString(content, "protocol: "+newProtocol)
		os.WriteFile(configFile, []byte(newContent), 0600)
	}
}

// isProtocolBlockedError 检查日志是否显示协议被阻断
func isProtocolBlockedError(logPath string) bool {
	content := readLogTail(logPath, 64<<10)
	if content == "" {
		return false
	}

	blockedPatterns := []string{
		"i/o timeout",
		"connection refused",
		"Unable to establish connection",
		"DialContext error",
		"timeout awaiting response",
		"no such host",
		"connection reset by peer",
	}

	for _, pattern := range blockedPatterns {
		if strings.Contains(content, pattern) {
			return true
		}
	}
	return false
}

// runTunnelSilent 静默运行隧道（日志进文件，终端保持交互）
// 特点：不阻塞菜单，按任意键返回，隧道继续后台运行
// [修改] 支持协议自动检测和智能切换，当首选协议失败时自动尝试其他协议
func runTunnelSilent(tunnel Tunnel, binPath string, useKeyboard bool, reader *bufio.Reader) bool {
	configPath := filepath.Join(homeDir, ".cloudflared")

	// 验证隧道配置
	if err := validateTunnelConsistency(binPath, configPath, &tunnel); err != nil {
		fmt.Printf("%s[X] 隧道验证失败: %v%s\n", ColorRed, err, ColorReset)
		waitForReturn(useKeyboard, reader)
		return false
	}

	// 停止已存在的同名隧道
	stopTunnel(tunnel.Name)
	stopDockerTunnel(tunnel.Name)

	// DNS 同步
	fullDomain := tunnel.Subdomain + "." + tunnel.Domain
	if err := forceSyncDNS(binPath, tunnel.Name, fullDomain, reader); err != nil {
		fmt.Printf("%s[!] DNS 同步警告: %v%s\n", ColorYellow, err, ColorReset)
	}

	logPath := getLogPath(tunnel.Name)
	pidPath := getPIDPath(tunnel.Name)
	configFile := filepath.Join(configPath, tunnel.Name+".yml")

	// 清理旧日志
	if _, err := os.Stat(logPath); err == nil {
		os.Remove(logPath)
	}

	// 检查配置文件
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		fmt.Printf("%s[X] 配置文件不存在: %s%s\n", ColorRed, configFile, ColorReset)
		waitForReturn(useKeyboard, reader)
		return true
	}

	// 显示启动信息（简洁版）
	fmt.Println(boxStyle.Render(
		"[ 启动隧道: " + tunnel.Name + " ]\n" +
			"[URL] https://" + fullDomain + " -> localhost:" + tunnel.Port,
	))

	// 检查本地服务
	if !checkLocalService(tunnel.Port) {
		fmt.Println(warningStyle.Render("[!] 警告: 端口 " + tunnel.Port + " 未检测到服务"))
		fmt.Println(infoStyle.Render("[*] 隧道将启动，但可能无法正常代理"))
	}

	// [修改] 智能协议切换：尝试多个协议直到成功
	protocols := []string{
		getProtocolForTunnel(tunnel.Name), // 用户偏好或上次成功的
		"quic",                            // 备选1：UDP协议，抗干扰
		"http2",                           // 备选2：TCP协议
		"auto",                            // 最后尝试自动选择
	}

	// 去重
	seen := make(map[string]bool)
	uniqueProtocols := []string{}
	for _, p := range protocols {
		if !seen[p] {
			seen[p] = true
			uniqueProtocols = append(uniqueProtocols, p)
		}
	}

	var started bool
	var lastErr error

	for i, protocol := range uniqueProtocols {
		// 如果不是第一次尝试，显示切换信息
		if i > 0 {
			fmt.Printf("%s[!] 协议 %s 连接失败，尝试 %s...%s\n",
				ColorYellow, uniqueProtocols[i-1], protocol, ColorReset)
		} else {
			fmt.Printf("%s[*] 使用协议: %s%s\n", ColorBlue, protocol, ColorReset)
		}

		// 更新配置文件中的协议
		updateConfigProtocol(configFile, protocol)
		saveProtocolPreference(tunnel.Name, protocol)

		// 清理之前的日志
		os.Remove(logPath)

		// 尝试启动
		success, err := tryStartSilent(tunnel, binPath, configFile, logPath, pidPath)
		if success {
			started = true
			if i > 0 {
				fmt.Printf("%s[OK] 协议 %s 启动成功！%s\n",
					ColorGreen, protocol, ColorReset)
			}
			break
		}

		lastErr = err

		// 检查是否是协议被阻断，如果不是则停止重试
		if !isProtocolBlockedError(logPath) {
			fmt.Printf("%s[X] 启动失败（非网络问题），停止重试%s\n", ColorRed, ColorReset)
			break
		}
	}

	if !started {
		fmt.Printf("%s[X] 所有协议均无法连接%s\n", ColorRed, ColorReset)
		if lastErr != nil {
			fmt.Printf("   错误: %v\n", lastErr)
		}
		fmt.Printf("   日志: %s\n", logPath)
		waitForReturn(useKeyboard, reader)
		return false
	}

	// 显示成功信息
	fmt.Printf("\n%s[OK] 隧道已静默启动%s\n", ColorGreen, ColorReset)
	fmt.Printf("   名称: %s%s%s\n", ColorCyan, tunnel.Name, ColorReset)
	fmt.Printf("   地址: https://%s%s.%s%s\n", ColorCyan, tunnel.Subdomain, tunnel.Domain, ColorReset)

	// 读取PID（tryStartSilent已保存）
	pid := getDaemonPID(tunnel.Name)
	if pid > 0 {
		fmt.Printf("   PID:  %s%d%s\n", ColorCyan, pid, ColorReset)
	}

	fmt.Printf("   日志: %s%s%s\n", ColorBlue, logPath, ColorReset)
	fmt.Printf("\n%s[*] 提示: 使用 [7] 后台管理 可查看日志和状态%s\n", ColorBlue, ColorReset)

	// 等待用户按键返回菜单（隧道继续运行）
	fmt.Printf("\n%s[按任意键返回主菜单...]%s", ColorYellow, ColorReset)

	if useKeyboard {
		// 重新打开 keyboard 等待单次按键
		if err := keyboard.Open(); err == nil {
			defer keyboard.Close()
			keyboard.GetKey() // 等待任意键
		} else {
			reader.ReadString('\n')
		}
	} else {
		reader.ReadString('\n')
	}

	return true
}

// tryStartSilent 单次尝试静默启动隧道
// 返回 (是否成功, 错误信息)
func tryStartSilent(tunnel Tunnel, binPath, configFile, logPath, pidPath string) (bool, error) {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	args := []string{"tunnel", "--config", configFile, "run", tunnel.Name}
	cmd := exec.Command(cloudflaredPath, args...)
	procMgr.SetupDaemonProcess(cmd)

	// 打开日志文件
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return false, fmt.Errorf("无法创建日志文件: %v", err)
	}

	// 重定向输出到日志文件
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	// 启动进程
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return false, fmt.Errorf("启动失败: %v", err)
	}

	// 保存 PID
	os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	// 等待验证启动（比后台启动更短的等待时间）
	time.Sleep(2 * time.Second)

	// 检查进程是否还在运行
	if !isProcessRunning(cmd.Process.Pid) {
		logFile.Close()
		return false, fmt.Errorf("进程启动后立即退出")
	}

	// 检查是否成功连接或出现协议错误
	for i := 0; i < 8; i++ { // 最多等待约6秒
		time.Sleep(800 * time.Millisecond)

		// 成功标志
		if checkTunnelConnected(logPath) {
			logFile.Close()
			return true, nil
		}

		// 协议阻断错误，快速失败
		if isProtocolBlockedError(logPath) {
			procMgr.KillProcess(cmd.Process.Pid, false)
			time.Sleep(200 * time.Millisecond)
			os.Remove(pidPath)
			logFile.Close()
			return false, fmt.Errorf("协议连接被阻断")
		}

		// 其他致命错误
		if checkTunnelError(logPath) {
			procMgr.KillProcess(cmd.Process.Pid, false)
			os.Remove(pidPath)
			logFile.Close()
			return false, fmt.Errorf("隧道启动错误")
		}
	}

	// 超时但进程还在，视为可能成功（让用户后续观察）
	logFile.Close()
	return true, nil
}

// model 定义 Bubble Tea 的模型状态（备选交互模式）
type model struct {
	list       list.Model
	choice     string
	quitting   bool
	config     Config
	homeDir    string
	binPath    string
	configPath string
	state      string
}

// item 定义列表中的菜单项
type item struct {
	title, desc string
	emoji       string
	action      string
}

func (i item) Title() string       { return "[" + i.emoji + "] " + i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

var (
	titleStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00D4AA")).MarginLeft(2)
	itemStyle         = lipgloss.NewStyle().PaddingLeft(4)
	selectedItemStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("#00D4AA")).Bold(true)
	helpStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("#626262")).MarginTop(1).MarginLeft(2)
	errorStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B")).Bold(true)
	successStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#51CF66")).Bold(true)
	warningStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD93D")).Bold(true)
	infoStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("#74C0FC"))
	boxStyle          = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#00D4AA")).Padding(1, 2).Margin(1, 2)
)

// initialModel 初始化 Bubble Tea 模型（备选美观界面）
func initialModel(cfg Config, homeDir string) model {
	items := []list.Item{
		item{"快速启动", "使用上次的配置立即启动", "START", "quick"},
		item{"新建隧道", "创建新的域名穿透", "NEW", "new"},
		item{"管理隧道", "查看/编辑/删除/后台管理", "MNG", "manage"},
		item{"切换隧道", "选择并启动其他隧道", "SW", "switch"},
		item{"导入/导出", "备份或恢复配置", "IO", "backup"},
		item{"证书管理", "管理 HTTPS 证书", "CERT", "cert"},
		item{"后台管理", "查看/终止后台隧道", "BG", "daemon"},
		item{"删除配置", "清除所有本地配置", "DEL", "clean"},
		item{"退出", "关闭程序", "EXIT", "exit"},
	}

	l := list.New(items, list.NewDefaultDelegate(), 60, 16)
	l.Title = "[ CF Tunnel Manager " + Version + " ]"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.Styles.Title = titleStyle
	l.Styles.HelpStyle = helpStyle

	return model{
		list:       l,
		config:     cfg,
		homeDir:    homeDir,
		binPath:    filepath.Join(homeDir, binDir),
		configPath: filepath.Join(homeDir, configDir),
		state:      "menu",
	}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			i, ok := m.list.SelectedItem().(item)
			if ok {
				m.choice = i.action
				return m.handleChoice()
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// handleChoice 处理菜单选择，返回状态供主程序处理
func (m model) handleChoice() (tea.Model, tea.Cmd) {
	switch m.choice {
	case "quick", "new", "manage", "switch", "backup", "cert", "daemon":
		m.state = m.choice
		return m, tea.Quit
	case "clean":
		m.state = "clean"
		return m, tea.Quit
	case "exit":
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return "\n[ 再见！ ]\n"
	}
	return "\n" + m.list.View() + "\n" + helpStyle.Render("[↑↓] 导航 [Enter] 选择 [q/Esc] 退出")
}

// cleanOldLogs 清理指定时间之前的旧日志文件
// 参数 maxAge: 最大保留时间，超过此时间的日志将被删除
func cleanOldLogs(maxAge time.Duration) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}

	cleaned := 0
	now := time.Now()

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".log") {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if now.Sub(info.ModTime()) > maxAge {
				os.Remove(filepath.Join(logDir, entry.Name()))
				cleaned++
			}
		}
	}

	if cleaned > 0 {
		fmt.Printf("%s[*] 已自动清理 %d 个过期日志文件%s\n",
			ColorBlue, cleaned, ColorReset)
		time.Sleep(500 * time.Millisecond)
	}
}

// main 程序入口函数
// 负责初始化平台信息、加载配置、确保 cloudflared 已安装，并启动主菜单
func main() {
	// ==================== 【新增】Android IPv4 优先 DNS ====================
	if runtime.GOOS == "android" {
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, "udp4", "8.8.8.8:53")
			},
		}
	}
	platformInfo = getPlatformInfo()

	// 修复：使用全局变量赋值
	var err error
	homeDir, err = os.UserHomeDir()
	if err != nil {
		homeDir = "."
	}

	savePath := filepath.Join(homeDir, saveFile)

	// ==================== 【修改】配置先生成，下载失败也不丢配置 ====================
	var cfg Config
	if data, err := os.ReadFile(savePath); err == nil {
		// [修复] 解析失败时备份原文件并拒绝本次运行写回，防止空配置覆盖用户数据
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			bakPath := savePath + ".bak-" + time.Now().Format("20060102-150405")
			os.WriteFile(bakPath, data, 0600)
			configLoadFailed = true
			fmt.Printf("%s[X] 配置文件解析失败: %v%s\n", ColorRed, err, ColorReset)
			fmt.Printf("%s[!] 原文件已备份到: %s%s\n", ColorYellow, bakPath, ColorReset)
			fmt.Printf("%s[!] 本次运行不会写回配置文件，请修复后重启程序%s\n", ColorYellow, ColorReset)
			time.Sleep(3 * time.Second)
		}
	} else {
		// 首次运行：立即生成配置模板（无论下载是否成功）
		cfg = Config{
			Tunnels: []Tunnel{
				{
					Name:      "mytunnel",
					Domain:    "example.com",
					Subdomain: "app",
					Port:      "8080",
					TunnelID:  "",
					Active:    true,
				},
			},
			APICredentials: &APICredentials{
				Token:     "",
				AccountID: "",
			},
		}

		// 用 yaml.Marshal 生成与 cfg 一致的模板，再追加注释
		data, _ := yaml.Marshal(cfg)
		template := string(data) + `
# === 配置说明 ===
# 1. 修改上方示例参数为你的真实信息（域名、子域名、端口、隧道名称）
# 2. 无浏览器环境(Android/iOS/Docker)请预填以下 API Token，桌面端可留空
#    Token 获取: https://dash.cloudflare.com/profile/api-tokens
#    所需权限: Account: Cloudflare Tunnel: Edit + Zone: DNS: Edit
#    Account ID 在 Cloudflare 仪表盘右侧面板获取
# 3. 保存后使用菜单 [2]新建隧道 或 [3]管理隧道 进行编辑/启动
`

		os.WriteFile(savePath, []byte(template), 0600)
		fmt.Printf("%s[OK] 配置模板已生成: %s%s\n", ColorGreen, savePath, ColorReset)
		fmt.Println(infoStyle.Render("[*] 若下载失败，可手动配置 API Token 后重试"))
	}

	// 下载失败不阻塞，配置已就绪，允许用户手动放置二进制后继续使用
	ensureCloudflared(homeDir)
	os.MkdirAll(logDir, 0755)
	os.MkdirAll(pidDir, 0755)
	cleanOldLogs(7 * 24 * time.Hour)

	useKeyboard := true
	reader := bufio.NewReader(os.Stdin)

	// VPS 优化：检测键盘支持，失败时自动降级为数字输入模式
	if err := keyboard.Open(); err != nil {
		useKeyboard = false
		fmt.Printf("%s[检测] 终端不支持方向键，已启用数字输入模式%s\n\n", ColorYellow, ColorReset)
	} else {
		keyboard.Close()
		time.Sleep(100 * time.Millisecond)
	}

	// [修复] 捕获中断信号，退出前恢复终端状态
	// 防止 Ctrl+C / kill 后终端停留在 raw 模式（不回显、按键乱码）
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println()
		resetTerminal()
		fmt.Println(ColorRed + "[ 已退出 ]" + ColorReset)
		os.Exit(130)
	}()

	showTraditionalMenu(cfg, homeDir, useKeyboard, reader)
}

// fixAndroidDNS 检测外网解析能力，失败时自动修复 Termux 的 resolv.conf
func fixAndroidDNS() {
	// 【新增】日期缓存：今天已修复过且 DNS 仍正常则跳过
	cachePath := filepath.Join(os.Getenv("HOME"), ".cloudflared-dns-fixed")
	if data, err := os.ReadFile(cachePath); err == nil {
		if strings.TrimSpace(string(data)) == time.Now().Format("2006-01-02") {
			return // 【修改】直接信任缓存，不再联网验证
		}
	}

	// 快速探测：能否解析 github.com
	if _, err := net.LookupHost("github.com"); err == nil {
		// 无需修复，写入缓存避免下次再测
		os.WriteFile(cachePath, []byte(time.Now().Format("2006-01-02")), 0644)
		return
	}

	fmt.Printf("%s[!] DNS 解析失败，尝试自动修复...%s\n", ColorYellow, ColorReset)

	prefix := os.Getenv("PREFIX")
	if prefix == "" {
		prefix = "/data/data/com.termux/files/usr"
	}
	resolvPath := filepath.Join(prefix, "etc", "resolv.conf")

	// 备份原文件
	if data, err := os.ReadFile(resolvPath); err == nil && len(data) > 0 {
		backupPath := resolvPath + ".backup." + time.Now().Format("20060102")
		os.WriteFile(backupPath, data, 0644)
	}

	// 写入纯 IPv4 DNS
	dnsContent := "nameserver 8.8.8.8\nnameserver 1.1.1.1\nnameserver 223.5.5.5\n"
	if err := os.WriteFile(resolvPath, []byte(dnsContent), 0644); err == nil {
		fmt.Printf("%s[OK] 已修复 DNS: %s%s\n", ColorGreen, resolvPath, ColorReset)
		os.WriteFile(cachePath, []byte(time.Now().Format("2006-01-02")), 0644)
	} else {
		fmt.Printf("%s[X] 无法写入 DNS 配置: %v%s\n", ColorRed, err, ColorReset)
	}
}

// ==================== 【新增】Android 本地代理自动探测 ====================
// detectProxy 尝试连接常见的本地代理端口，仅 Android 平台生效
func detectProxy() string {
	if runtime.GOOS != "android" {
		return ""
	}

	// 常见代理端口快速探测（300ms 超时）
	ports := []string{
		"127.0.0.1:7890", // Clash/Mihomo 默认
		"127.0.0.1:7891", // 备用
		"127.0.0.1:1080", // SS/SSR/V2ray
		"127.0.0.1:1081", // 备用
	}
	for _, addr := range ports {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return "http://" + addr
		}
	}
	return ""
}

// =======================================================================

// ensureCloudflared 确保 cloudflared 二进制文件已安装
// 检查本地 bin 目录，如果不存在则自动下载对应平台的版本
func ensureCloudflared(homeDir string) {
	localBin := filepath.Join(homeDir, binDir)
	cloudflaredBin := filepath.Join(localBin, platformInfo.BinaryName)

	os.MkdirAll(localBin, 0755)
	os.MkdirAll(filepath.Join(homeDir, configDir), 0755)

	// ==================== 【关键修改】先查二进制，存在直接返回 ====================
	if info, err := os.Stat(cloudflaredBin); err == nil {
		// 二进制已存在：静默修复权限（如有必要），然后直接返回
		if info.Mode().Perm()&0111 == 0 {
			os.Chmod(cloudflaredBin, 0755)
		}
		return // ← 存在就直接用，不碰任何网络/DNS
	}
	// ==============================================================================

	// 以下仅在二进制不存在时执行（首次运行或文件被删除）

	if os.Getenv("CF_SKIP_DOWNLOAD") == "1" {
		fmt.Printf("%s[*] 跳过下载检测 (CF_SKIP_DOWNLOAD=1)%s\n", ColorBlue, ColorReset)
		return
	}

	// 需要下载时才修复 DNS，避免每次启动都卡顿
	if runtime.GOOS == "android" {
		fixAndroidDNS()
	}

	// 今日失败标记检查
	failMark := filepath.Join(homeDir, configDir, ".download-failed-"+time.Now().Format("20060102"))
	if _, err := os.Stat(failMark); err == nil {
		fmt.Printf("%s[*] 今日已尝试下载且失败，跳过网络检测%s\n", ColorYellow, ColorReset)
		time.Sleep(1 * time.Second)
		return
	}

	fmt.Printf("%s[*] 首次运行，平台: %s%s\n",
		infoStyle.Render(""), platformInfo.String(), ColorReset)
	fmt.Printf("%s[*] 目标文件: %s%s\n",
		infoStyle.Render(""), cloudflaredBin, ColorReset)
	fmt.Printf("%s[*] 下载地址: %s%s\n",
		infoStyle.Render(""), platformInfo.DownloadURL, ColorReset)
	fmt.Printf("%s[*] 正在下载 cloudflared...%s\n",
		infoStyle.Render(""), ColorReset)

	if err := downloadCloudflared(cloudflaredBin, filepath.Join(homeDir, configDir)); err != nil {
		fmt.Printf("%s[X] 下载失败: %v%s\n", ColorRed, err, ColorReset)
		os.WriteFile(failMark, []byte(err.Error()), 0644)

		if runtime.GOOS == "android" {
			fmt.Printf("%s[*] 可设置代理后重试，如: export HTTP_PROXY=http://127.0.0.1:7890%s\n",
				ColorBlue, ColorReset)
		}
		time.Sleep(2 * time.Second)
		return
	}

	addToPath(localBin)
	fmt.Println(successStyle.Render("[OK] 安装完成"))
	time.Sleep(1 * time.Second)
}

// downloadMirrors 下载镜像前缀列表（应对 GitHub 直连受限的环境）
// 前缀直接拼接在 GitHub 原始 URL 之前
var downloadMirrors = []string{
	"",                      // 直连
	"https://ghfast.top/",   // ghfast 镜像
	"https://gh-proxy.com/", // gh-proxy 镜像
	"https://ghproxy.net/",  // ghproxy 镜像
}

// downloadCloudflared 下载并安装 cloudflared 二进制文件
// 安全措施：
//  1. 先下载到临时文件（dest+".tmp"），全部校验通过后再原子重命名，避免截断文件被当作合法二进制
//  2. 拒绝 text/html 响应（劫持/错误页）和小于 1MB 的文件
//  3. 校验实际写入字节数与 Content-Length 一致（服务端提供时）
//  4. 尝试获取官方 .sha256 校验和（可选，获取失败不阻断）
//  5. 多个镜像依次回退
func downloadCloudflared(dest, marksDir string) error {
	fmt.Printf("%s[*] 下载中... ", infoStyle.Render(""))

	// 优先读取环境变量（所有平台通用）
	proxyURL := os.Getenv("HTTP_PROXY")
	if proxyURL == "" {
		proxyURL = os.Getenv("http_proxy")
	}
	if proxyURL == "" {
		proxyURL = os.Getenv("HTTPS_PROXY")
	}
	if proxyURL == "" {
		proxyURL = os.Getenv("https_proxy")
	}

	// 【新增】仅 Android 自动探测本地代理端口
	if proxyURL == "" && runtime.GOOS == "android" {
		if detected := detectProxy(); detected != "" {
			proxyURL = detected
			fmt.Printf("%s[*] 自动检测到本地代理: %s%s\n", ColorBlue, proxyURL, ColorReset)
		}
	}

	// 构建 Transport：有探测到的代理直接用，否则回退到环境变量
	transport := &http.Transport{}
	if proxyURL != "" {
		if proxyParsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxyParsed)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}

	// 超时覆盖整个下载（大文件 + 慢网络给足时间，避免旧版 15s 截断问题）
	client := &http.Client{
		Timeout:   10 * time.Minute,
		Transport: transport,
	}

	tmpDest := dest + ".tmp"
	defer os.Remove(tmpDest) // 无论成功失败，清理临时文件

	isTgz := platformInfo.OS == "darwin" && strings.HasSuffix(platformInfo.DownloadURL, ".tgz")

	var lastErr error
	for _, mirror := range downloadMirrors {
		binURL := mirror + platformInfo.DownloadURL
		if mirror != "" {
			fmt.Printf("\n%s[*] 尝试镜像: %s%s\n", ColorBlue, mirror, ColorReset)
		}

		var err error
		if isTgz {
			// macOS：先下载 tgz 到临时文件，再解压到 dest+".tmp"
			tmpTgz := dest + ".tmp.tgz"
			os.Remove(tmpTgz)
			err = downloadBinaryToFile(client, binURL, tmpTgz)
			if err == nil {
				err = extractTgzToFile(tmpTgz, tmpDest)
			}
			os.Remove(tmpTgz)
		} else {
			err = downloadBinaryToFile(client, binURL, tmpDest)
		}
		if err != nil {
			fmt.Printf("%s[X] 下载失败: %v%s\n", ColorRed, err, ColorReset)
			lastErr = err
			continue
		}

		// 可选 SHA256 校验：校验文件拿不到就跳过，校验不匹配视为下载失败换镜像
		if vErr := verifySha256(client, binURL+".sha256", tmpDest); vErr != nil {
			fmt.Printf("%s[X] 校验失败: %v%s\n", ColorRed, vErr, ColorReset)
			lastErr = vErr
			continue
		}

		// 原子替换：rename 成功前 dest 不会被破坏
		if err := os.Rename(tmpDest, dest); err != nil {
			return fmt.Errorf("替换二进制失败: %v", err)
		}
		if chmodErr := os.Chmod(dest, 0755); chmodErr != nil && runtime.GOOS != "windows" {
			return fmt.Errorf("设置可执行权限失败: %v", chmodErr)
		}

		// 下载成功：清理历史失败标记
		clearDownloadFailMarks(marksDir)
		return nil
	}

	return fmt.Errorf("所有下载源均失败，最后错误: %v", lastErr)
}

// downloadBinaryToFile 下载 URL 到 dest，并做基本完整性检查
// 拒绝 HTML 错误页（可能是劫持/门户认证页）和明显不完整的文件
func downloadBinaryToFile(client *http.Client, url, dest string) error {
	resp, err := client.Get(url)
	if err != nil {
		// 【保留】仅 Android 且使用了代理时，提示检查代理
		if runtime.GOOS == "android" {
			if proxyURL := os.Getenv("HTTP_PROXY"); proxyURL != "" {
				return fmt.Errorf("通过代理 %s 下载失败: %v (请检查 Clash/VPN 是否正常运行)", proxyURL, err)
			}
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// 拒绝 HTML 响应（劫持页/错误页常以 200 + text/html 返回）
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/html") {
		return fmt.Errorf("返回内容为 HTML 页面（可能是网络劫持或错误页），已拒绝保存")
	}

	const minBinSize = 1 << 20 // 1MB：cloudflared 二进制远大于此
	if resp.ContentLength >= 0 && resp.ContentLength < minBinSize {
		return fmt.Errorf("文件过小 (%d 字节)，可能不是有效的 cloudflared 二进制", resp.ContentLength)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("下载中断: %v (已删除临时文件)", err)
	}
	if written < minBinSize {
		return fmt.Errorf("文件不完整 (%d 字节 < 1MB)", written)
	}
	if resp.ContentLength > 0 && written != resp.ContentLength {
		return fmt.Errorf("字节数不符: 期望 %d, 实际 %d", resp.ContentLength, written)
	}
	return nil
}

// verifySha256 尝试获取官方 .sha256 文件并校验
// 校验文件下载/解析失败时返回 nil（可选校验，不阻断安装）；哈希不匹配返回错误
func verifySha256(client *http.Client, shaURL, filePath string) error {
	resp, err := client.Get(shaURL)
	if err != nil {
		return nil // 拿不到校验文件，跳过校验
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return nil
	}
	expected, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(fields[0])))
	if err != nil || len(expected) != sha256.Size {
		return nil // 格式无法识别，跳过校验
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !bytes.Equal(h.Sum(nil), expected) {
		return fmt.Errorf("SHA256 不匹配")
	}
	fmt.Printf("%s[OK] SHA256 校验通过%s\n", ColorGreen, ColorReset)
	return nil
}

// clearDownloadFailMarks 清理 .download-failed-* 失败标记
func clearDownloadFailMarks(marksDir string) {
	entries, err := os.ReadDir(marksDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".download-failed-") {
			os.Remove(filepath.Join(marksDir, entry.Name()))
		}
	}
}

// extractTgzToFile 解压 tgz 中的 cloudflared 到 dest（临时文件，尚未生效）
func extractTgzToFile(tgzPath, dest string) error {
	os.Remove(dest)
	cmd := exec.Command("tar", "-xzf", tgzPath, "-C", filepath.Dir(dest))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("解压 tgz 失败: %v", err)
	}
	extractedPath := filepath.Join(filepath.Dir(dest), "cloudflared")
	if _, err := os.Stat(extractedPath); err != nil {
		return fmt.Errorf("解压后未找到 cloudflared 可执行文件")
	}
	return os.Rename(extractedPath, dest)
}

// runWithDocker 使用 Docker 运行隧道
// 创建守护容器，自动重启，挂载配置目录
func runWithDocker(tunnel Tunnel) error {
	fmt.Println(infoStyle.Render("[*] 使用 Docker 运行 cloudflared..."))

	if err := exec.Command("docker", "--version").Run(); err != nil {
		return fmt.Errorf("Docker 未安装")
	}

	cmd := exec.Command("docker", "run", "-d", "--name", "cf-tunnel-"+tunnel.Name,
		"--restart", "unless-stopped",
		"-v", filepath.Join(homeDir, ".cloudflared")+":/etc/cloudflared",
		"cloudflare/cloudflared:latest",
		"tunnel", "run",
		"--config", "/etc/cloudflared/"+tunnel.Name+".yml",
		tunnel.Name)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// stopDockerTunnel 停止并删除 Docker 容器
func stopDockerTunnel(name string) {
	exec.Command("docker", "stop", "cf-tunnel-"+name).Run()
	exec.Command("docker", "rm", "cf-tunnel-"+name).Run()
}

// checkCloudflareTunnelExists 检查云端是否存在指定名称的隧道
// 返回隧道 ID 和存在状态
func checkCloudflareTunnelExists(binPath, tunnelName string) (string, bool) {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	var cmd *exec.Cmd

	if platformInfo.IsDocker {
		cmd = exec.Command("docker", "run", "--rm",
			"-v", filepath.Join(homeDir, ".cloudflared")+":/etc/cloudflared",
			"cloudflare/cloudflared:latest", "tunnel", "list")
	} else {
		cmd = exec.Command(cloudflaredPath, "tunnel", "list")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, tunnelName) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[0], true
			}
		}
	}
	return "", false
}

// deleteCloudflareTunnel 删除云端的隧道
// 修复：Docker 命令添加 -it 参数以支持交互，或确保正确捕获输出
func deleteCloudflareTunnel(binPath, tunnelName string) error {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	var cmd *exec.Cmd

	if platformInfo.IsDocker {
		// 修复：添加 -t 参数分配伪终端，某些情况下需要
		// 或者使用 -i 确保 stdin 保持打开
		cmd = exec.Command("docker", "run", "--rm", "-i",
			"-v", filepath.Join(homeDir, ".cloudflared")+":/etc/cloudflared",
			"cloudflare/cloudflared:latest", "tunnel", "delete", "-f", tunnelName)
	} else {
		cmd = exec.Command(cloudflaredPath, "tunnel", "delete", "-f", tunnelName)
	}

	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	// 检查是否是"未找到"错误，这种不算失败
	if err != nil && !strings.Contains(outputStr, "not found") &&
		!strings.Contains(outputStr, "No tunnel") &&
		!strings.Contains(outputStr, "could not find") {
		return fmt.Errorf("删除失败: %v, 输出: %s", err, outputStr)
	}

	// 即使返回错误，但输出包含 not found，也视为成功（已经不存在了）
	if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No tunnel") {
		return nil
	}

	return nil
}

// getTunnelIDByName 通过隧道名称获取 UUID
// 优先尝试 JSON 输出格式，失败时回退到文本解析
func getTunnelIDByName(binPath, tunnelName string) (string, error) {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	configPath := filepath.Join(homeDir, ".cloudflared")

	var cmd *exec.Cmd
	if platformInfo.IsDocker {
		cmd = exec.Command("docker", "run", "--rm",
			"-v", configPath+":/etc/cloudflared",
			"cloudflare/cloudflared:latest", "tunnel", "list", "--output", "json")
	} else {
		cmd = exec.Command(cloudflaredPath, "tunnel", "list", "--output", "json")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return getTunnelIDFromText(binPath, tunnelName)
	}

	var tunnels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	if err := json.Unmarshal(output, &tunnels); err == nil {
		for _, t := range tunnels {
			if t.Name == tunnelName {
				if isValidUUID(t.ID) {
					return t.ID, nil
				}
			}
		}
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var t struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}

		if err := json.Unmarshal([]byte(line), &t); err == nil {
			if t.Name == tunnelName {
				if isValidUUID(t.ID) {
					return t.ID, nil
				}
			}
		}
	}

	return getTunnelIDFromText(binPath, tunnelName)
}

// getTunnelIDFromText 从文本格式的隧道列表中解析 UUID
// 位置：约第 1142 行（确保只定义一次）
// 修复：改进解析逻辑，处理不同输出格式
func getTunnelIDFromText(binPath, tunnelName string) (string, error) {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)

	// 先尝试带 --output text 参数
	cmd := exec.Command(cloudflaredPath, "tunnel", "list", "--output", "text")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// 尝试不带 --output 参数（旧版本兼容）
		cmd = exec.Command(cloudflaredPath, "tunnel", "list")
		output, err = cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("执行 tunnel list 失败: %v, 输出: %s", err, string(output))
		}
	}

	lines := strings.Split(string(output), "\n")

	for i, line := range lines {
		line = strings.TrimSpace(line)
		// 跳过空行和标题行
		if i == 0 || line == "" || strings.HasPrefix(line, "ID") ||
			strings.HasPrefix(line, "You can") || strings.HasPrefix(line, "NAME") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 {
			// 检查是否包含 tunnelName
			nameIndex := -1
			idIndex := -1

			for j, field := range fields {
				if field == tunnelName {
					nameIndex = j
				}
				if isValidUUID(field) && idIndex == -1 {
					idIndex = j
				}
			}

			// 如果找到名称，且找到 UUID，返回 UUID
			if nameIndex != -1 && idIndex != -1 {
				return fields[idIndex], nil
			}

			// 简单匹配：如果行包含 tunnelName，返回第一个 UUID
			if strings.Contains(line, tunnelName) {
				for _, field := range fields {
					if isValidUUID(field) {
						return field, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("在输出中未找到隧道 %s", tunnelName)
}

// isValidUUID 验证字符串是否为标准 UUID 格式
func isValidUUID(s string) bool {
	match, _ := regexp.MatchString(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`, s)
	return match
}

// isBrowserAvailable 检测当前环境是否支持浏览器 OAuth 回调
// Android/iOS：应用沙箱隔离，浏览器回调永远到不了终端进程 → 不可用
// Docker：无浏览器环境 → 不可用
// Linux/macOS/Windows：cloudflared 会输出授权 URL 到终端，用户可自行处理 → 可用
func isBrowserAvailable() bool {
	if runtime.GOOS == "android" || runtime.GOOS == "ios" {
		return false
	}
	if platformInfo.IsDocker {
		return false
	}
	return true
}

// hasAPICredentials 检查是否已配置 API Token
func (cfg *Config) hasAPICredentials() bool {
	return cfg.APICredentials != nil &&
		cfg.APICredentials.Token != "" &&
		cfg.APICredentials.AccountID != ""
}

// createTunnelViaAPI 使用 Cloudflare REST API 直接创建隧道并生成凭证文件
// 适用于无浏览器环境（Android/Termux/Docker）
func createTunnelViaAPI(cfg *Config, tunnel *Tunnel, configPath string) error {
	creds := cfg.APICredentials
	if creds == nil {
		return fmt.Errorf("未配置 API Token")
	}

	// 1. 调用 API 创建隧道
	apiURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel", creds.AccountID)
	// [安全] 用 json.Marshal 构造请求体，避免名称中的特殊字符破坏 JSON
	reqBodyBytes, err := json.Marshal(map[string]string{
		"name":       tunnel.Name,
		"config_src": "local",
	})
	if err != nil {
		return fmt.Errorf("构造请求失败: %v", err)
	}

	req, _ := http.NewRequest("POST", apiURL, bytes.NewReader(reqBodyBytes))
	req.Header.Set("Authorization", "Bearer "+creds.Token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API 请求失败: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("创建隧道失败 HTTP %d: %s", resp.StatusCode, string(body))
	}

	// 2. 解析响应
	var apiResp struct {
		Success bool `json:"success"`
		Result  struct {
			ID              string `json:"id"`
			CredentialsFile struct {
				AccountTag   string `json:"AccountTag"`
				TunnelID     string `json:"TunnelID"`
				TunnelName   string `json:"TunnelName"`
				TunnelSecret string `json:"TunnelSecret"`
			} `json:"credentials_file"`
		} `json:"result"`
	}

	if err := json.Unmarshal(body, &apiResp); err != nil || !apiResp.Success {
		return fmt.Errorf("解析 API 响应失败: %v", err)
	}

	tunnel.TunnelID = apiResp.Result.ID

	// 3. 写入 {tunnelID}.json 凭证文件（cloudflared run 的唯一依赖）
	credFile := filepath.Join(configPath, tunnel.TunnelID+".json")
	credData, _ := json.MarshalIndent(map[string]string{
		"AccountTag":   apiResp.Result.CredentialsFile.AccountTag,
		"TunnelSecret": apiResp.Result.CredentialsFile.TunnelSecret,
		"TunnelID":     apiResp.Result.CredentialsFile.TunnelID,
		"TunnelName":   apiResp.Result.CredentialsFile.TunnelName,
	}, "", "  ")

	if err := os.WriteFile(credFile, credData, 0600); err != nil {
		return fmt.Errorf("写入凭证文件失败: %v", err)
	}

	// 4. 通过 API 创建 DNS 记录（替代 cloudflared route dns）
	zoneID, err := getZoneIDByDomain(creds.Token, tunnel.Domain)
	if err != nil {
		return fmt.Errorf("获取 Zone ID 失败: %v", err)
	}

	dnsURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
	// [安全] 用 json.Marshal 构造请求体，避免子域名中的特殊字符破坏 JSON
	dnsBodyBytes, err := json.Marshal(map[string]interface{}{
		"type":    "CNAME",
		"name":    tunnel.Subdomain,
		"content": tunnel.TunnelID + ".cfargotunnel.com",
		"proxied": true,
		"ttl":     1,
	})
	if err != nil {
		return fmt.Errorf("构造 DNS 请求失败: %v", err)
	}

	dnsReq, _ := http.NewRequest("POST", dnsURL, bytes.NewReader(dnsBodyBytes))
	dnsReq.Header.Set("Authorization", "Bearer "+creds.Token)
	dnsReq.Header.Set("Content-Type", "application/json")

	dnsResp, err := client.Do(dnsReq)
	if err != nil {
		return fmt.Errorf("DNS 记录创建失败: %v", err)
	}
	defer dnsResp.Body.Close()

	if dnsResp.StatusCode != 200 && dnsResp.StatusCode != 201 {
		dnsBodyBytes, _ := io.ReadAll(dnsResp.Body)
		if !strings.Contains(string(dnsBodyBytes), "already exists") &&
			!strings.Contains(string(dnsBodyBytes), "81053") {
			return fmt.Errorf("DNS 创建失败 HTTP %d: %s", dnsResp.StatusCode, string(dnsBodyBytes))
		}
	}

	return nil
}

// getZoneIDByDomain 通过域名查询 Zone ID
func getZoneIDByDomain(apiToken, domain string) (string, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?name=%s", domain)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Success bool `json:"success"`
		Result  []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil || !result.Success || len(result.Result) == 0 {
		return "", fmt.Errorf("未找到域名 %s 的 Zone", domain)
	}
	return result.Result[0].ID, nil
}

// normalizeServiceScheme 归一化 ingress service scheme；空值保持旧版 http 行为
func normalizeServiceScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "ssh":
		return "ssh"
	case "http", "":
		return "http"
	case "https":
		return "https"
	default:
		return "http"
	}
}

// tunnelServiceURL 生成 cloudflared ingress.service。
// 旧配置 ServiceScheme 为空时与原版完全一致：http://localhost:<port>
func tunnelServiceURL(t Tunnel) string {
	return fmt.Sprintf("%s://localhost:%s", normalizeServiceScheme(t.ServiceScheme), t.Port)
}

// startLocalSSHTunnelPreset 仅做参数预置后复用原有 createNewTunnel 流程，
// 不绕过原有的登录/API凭证、隧道创建、DNS同步、启动选项逻辑。
func startLocalSSHTunnelPreset(cfg *Config, configPath, binPath string, useKeyboard bool, reader *bufio.Reader) {
	keyboard.Close()
	time.Sleep(100 * time.Millisecond)

	if !checkLocalService("22") {
		fmt.Println(warningStyle.Render("[!] 未检测到本机 22 端口 SSH 服务，仍将创建配置"))
		fmt.Println(infoStyle.Render("[*] 可先确认: ss -lnt | grep ':22' 或 systemctl status ssh/sshd"))
	}

	fmt.Println(boxStyle.Render(
		"[ 本机 SSH 穿透 ]\n" +
			"将创建 service=ssh://localhost:22 的隧道。\n" +
			"远程端仍需 cloudflared，示例:\n" +
			"  ssh -o ProxyCommand='cloudflared access ssh --hostname %h' user@域名",
	))

	presetServiceScheme, presetPort, presetName = "ssh", "22", "ssh-local"
	defer func() {
		presetServiceScheme, presetPort, presetName = "", "", ""
		resetTerminal()
	}()

	createNewTunnel(cfg, configPath, binPath, useKeyboard, reader)
}

// validateTunnelConsistency 验证并修复本地隧道配置与云端的一致性
// 检查 TunnelID 是否匹配，确保凭证文件存在（自动重新获取），重写配置文件
// [修改] 支持动态协议选择和自动切换
func validateTunnelConsistency(binPath, configPath string, tunnel *Tunnel) error {
	// 【新增】API Token 模式：如果已有 TunnelID 且凭证文件存在，直接生成配置
	if tunnel.TunnelID != "" {
		credFile := filepath.Join(configPath, tunnel.TunnelID+".json")
		if _, err := os.Stat(credFile); err == nil {
			protocol := getProtocolForTunnel(tunnel.Name)
			fullDomain := tunnel.Subdomain + "." + tunnel.Domain
			configFile := filepath.Join(configPath, tunnel.Name+".yml")
			configContent := fmt.Sprintf(
				`tunnel: %s
credentials-file: %s
protocol: %s

ingress:
  - hostname: %s
    service: %s
  - service: http_status:404
`, tunnel.TunnelID, credFile, protocol, fullDomain, tunnelServiceURL(*tunnel))
			return os.WriteFile(configFile, []byte(configContent), 0600)
		}
	}

	// 【原有】浏览器模式：通过 cloudflared CLI 验证
	realID, err := getTunnelIDByName(binPath, tunnel.Name)
	if err != nil {
		return fmt.Errorf("无法验证隧道: %v", err)
	}

	if tunnel.TunnelID != "" && tunnel.TunnelID != realID {
		fmt.Printf("%s[!] 检测到 ID 不匹配: 本地 %s vs 云端 %s，自动修正...%s\n",
			ColorYellow, tunnel.TunnelID, realID, ColorReset)
		tunnel.TunnelID = realID
	} else if tunnel.TunnelID == "" {
		tunnel.TunnelID = realID
	}

	credFile := filepath.Join(configPath, tunnel.TunnelID+".json")

	// 如果凭证不存在，尝试重新获取
	if _, err := os.Stat(credFile); os.IsNotExist(err) {
		fmt.Printf("%s[!] 凭证文件缺失: %s，尝试重新获取...%s\n",
			ColorYellow, credFile, ColorReset)

		if err := fetchTunnelCredentials(binPath, configPath, tunnel.TunnelID); err != nil {
			return fmt.Errorf("凭证文件不存在且无法重新获取: %v", err)
		}

		if _, err := os.Stat(credFile); os.IsNotExist(err) {
			return fmt.Errorf("重新获取凭证后文件仍不存在: %s", credFile)
		}

		fmt.Printf("%s[✓] 凭证重新获取成功%s\n", ColorGreen, ColorReset)
	}

	// [修改] 智能协议选择：优先使用已保存的，否则默认 quic
	protocol := getProtocolForTunnel(tunnel.Name)
	fmt.Printf("%s[*] 使用协议: %s%s\n", ColorBlue, protocol, ColorReset)

	fullDomain := tunnel.Subdomain + "." + tunnel.Domain
	configFile := filepath.Join(configPath, tunnel.Name+".yml")
	configContent := fmt.Sprintf(
		`tunnel: %s
credentials-file: %s
protocol: %s

ingress:
  - hostname: %s
    service: %s
  - service: http_status:404
`, tunnel.TunnelID, credFile, protocol, fullDomain, tunnelServiceURL(*tunnel))

	return os.WriteFile(configFile, []byte(configContent), 0600)
}

// fetchTunnelCredentials 使用 cloudflared tunnel token 重新获取凭证
// 修复：使用正确的 JSON 字段名（小写开头）
func fetchTunnelCredentials(binPath, configPath, tunnelID string) error {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)

	// 检查 cloudflared 是否存在
	if _, err := os.Stat(cloudflaredPath); os.IsNotExist(err) {
		return fmt.Errorf("cloudflared 可执行文件不存在: %s", cloudflaredPath)
	}

	// 执行 cloudflared tunnel token <tunnel-id> 获取 base64 token
	cmd := exec.Command(cloudflaredPath, "tunnel", "token", tunnelID)
	output, err := cmd.Output()
	if err != nil {
		// 检查是否未登录
		if exitErr, ok := err.(*exec.ExitError); ok {
			errMsg := string(exitErr.Stderr)
			if strings.Contains(errMsg, "login") || strings.Contains(errMsg, "Unauthorized") || strings.Contains(errMsg, "authentication") {
				return fmt.Errorf("Cloudflare 未授权，请先执行 'cloudflared tunnel login'")
			}
		}
		return fmt.Errorf("获取 tunnel token 失败: %v", err)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return fmt.Errorf("获取到的 token 为空")
	}

	// 解码 base64 token 为 JSON
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("解码 base64 token 失败: %v", err)
	}

	// 验证解码后的内容是否为有效 JSON
	var credData map[string]interface{}
	if err := json.Unmarshal(decoded, &credData); err != nil {
		return fmt.Errorf("解码的 token 不是有效的 JSON 格式: %v", err)
	}

	// 修复：cloudflared 使用小写字段名
	// 字段可能是: AccountTag, TunnelSecret, TunnelID (首字母大写)
	// 或者是: a, s, t (首字母小写，来自 base64 token)
	requiredFields := [][]string{
		{"AccountTag", "TunnelSecret", "TunnelID"}, // 旧格式
		{"a", "s", "t"}, // 新格式（token 解码后）
	}

	validFormat := false
	for _, fields := range requiredFields {
		hasAll := true
		for _, field := range fields {
			if _, ok := credData[field]; !ok {
				hasAll = false
				break
			}
		}
		if hasAll {
			validFormat = true
			break
		}
	}

	if !validFormat {
		return fmt.Errorf("凭证缺少必要字段，可用字段: %v", getMapKeys(credData))
	}

	// 如果是新格式（a,s,t），转换为旧格式（AccountTag, TunnelSecret, TunnelID）
	// 以便 cloudflared 能正确识别
	if _, ok := credData["a"]; ok {
		converted := map[string]interface{}{
			"AccountTag":   credData["a"],
			"TunnelSecret": credData["s"],
			"TunnelID":     credData["t"],
		}
		// 保留其他字段
		for k, v := range credData {
			if k != "a" && k != "s" && k != "t" {
				converted[k] = v
			}
		}
		decoded, _ = json.Marshal(converted)
	}

	// 保存凭证文件
	credFile := filepath.Join(configPath, tunnelID+".json")
	if err := os.WriteFile(credFile, decoded, 0600); err != nil {
		return fmt.Errorf("保存凭证文件失败: %v", err)
	}

	// 设置适当的文件权限（只允许所有者读写）
	if err := os.Chmod(credFile, 0600); err != nil {
		return fmt.Errorf("设置凭证文件权限失败: %v", err)
	}

	return nil
}

// getMapKeys 辅助函数：获取 map 的所有 key
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// readSecretLine 读取一行敏感输入（不回显）
// 终端环境用 term.ReadPassword；非终端环境回退到普通读取并提示风险
func readSecretLine(reader *bufio.Reader) string {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		data, err := term.ReadPassword(fd)
		fmt.Println() // ReadPassword 不回显也不换行，手动补一个
		if err == nil {
			return string(data)
		}
		fmt.Println(warningStyle.Render("[!] 无法关闭回显，输入将可见"))
	}
	line, _ := reader.ReadString('\n')
	return line
}

// promptAPICredentials 引导用户输入 API Token 与 Account ID（Token 不回显）
// 返回 true 表示凭证已保存
func promptAPICredentials(cfg *Config, reader *bufio.Reader) bool {
	fmt.Println(infoStyle.Render("[*] 请使用 Cloudflare API Token 模式："))
	fmt.Println()
	fmt.Println("  1) 登录 https://dash.cloudflare.com/profile/api-tokens")
	fmt.Println("  2) 创建 Token，权限：")
	fmt.Println("     - Account: Cloudflare Tunnel : Edit")
	fmt.Println("     - Zone: DNS : Edit")
	fmt.Println("  3) 获取 Account ID（仪表盘右侧面板）")
	fmt.Println()

	fmt.Print(infoStyle.Render("[API Token] (输入不回显): "))
	apiToken := strings.TrimSpace(readSecretLine(reader))

	fmt.Print(infoStyle.Render("[Account ID]: "))
	accountID, _ := reader.ReadString('\n')
	accountID = strings.TrimSpace(accountID)

	if apiToken == "" || accountID == "" {
		fmt.Println(errorStyle.Render("[X] 凭证不完整，已取消"))
		return false
	}

	cfg.APICredentials = &APICredentials{
		Token:     apiToken,
		AccountID: accountID,
	}
	saveConfig(*cfg, filepath.Join(homeDir, saveFile))
	fmt.Println(successStyle.Render("[OK] API 凭证已保存"))
	return true
}

// 隧道名/子域名合法性校验（防止路径逃逸与 JSON 注入）
var tunnelNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

// isValidTunnelName 校验隧道名/子域名：仅允许字母数字下划线连字符，1-63 字符
func isValidTunnelName(name string) bool {
	return tunnelNameRe.MatchString(name)
}

// isValidDomain 域名校验（宽松版）：至少一个点，无空白与斜杠
func isValidDomain(domain string) bool {
	if strings.ContainsAny(domain, " \t/\\") || !strings.Contains(domain, ".") {
		return false
	}
	return len(domain) <= 253
}

// createNewTunnel 交互式创建新隧道（VPS 优化版）
// 1. 移除所有 emoji，使用 ASCII 符号确保对齐
// 2. 修复 VPS 卡顿：后台/Docker 启动后立即返回，不阻塞键盘
// 3. 修复回车确认：仅前台运行和暂不启动需要回车，后台启动自动返回
// 参数 cfg: 配置指针（会被修改），configPath: 配置目录，binPath: 二进制目录
// waitForReturn 辅助函数：等待用户按键后返回
// 修复：统一处理 keyboard 和 reader 模式，正确处理 TTY 状态
func waitForReturn(useKeyboard bool, reader *bufio.Reader) {
	fmt.Printf("\n%s[按回车返回主菜单...]%s", ColorYellow, ColorReset)

	// 关键：强制刷新输出缓冲区
	os.Stdout.Sync()

	if useKeyboard {
		// 尝试打开 keyboard
		if err := keyboard.Open(); err == nil {
			defer keyboard.Close() // 确保最终关闭

			// 修复：循环读取直到获得回车键
			// 注意：keyboard 库只定义了 KeyEnter，没有 KeyReturn
			for {
				char, key, err := keyboard.GetKey()
				if err != nil {
					// 读取错误，回退到标准输入
					break
				}

				// 接受回车键（Enter）或字符 \r \n
				// 修复：移除不存在的 keyboard.KeyReturn
				if key == keyboard.KeyEnter || char == '\r' || char == '\n' {
					return // 正确获得回车，直接返回
				}
				// 忽略其他按键，继续等待回车
			}

			// 如果循环退出（出错），执行下方的 reader 回退
		}

		// Keyboard 打开失败或读取出错，回退到 reader
		// 关键：必须先重置终端到 cooked 模式
		resetTerminal()
	}

	// 使用标准输入读取
	reader.ReadString('\n')
}

// ====== 修复：createNewTunnel（VPS 优化版）======
// 1. 移除所有 emoji，使用 ASCII 符号确保对齐
// 2. 修复 VPS 卡顿：后台/Docker 启动后立即返回，不阻塞键盘
// 3. 修复回车确认：仅前台运行和暂不启动需要回车，后台启动自动返回
// 4. 修复 goto 跳入块错误：使用 waitForReturn 替代 goto CLEANUP
func createNewTunnel(cfg *Config, configPath, binPath string, useKeyboard bool, reader *bufio.Reader) {
	// VPS 优化：延迟重置键盘，避免状态混乱
	keyboard.Close()
	time.Sleep(100 * time.Millisecond)
	defer func() {
		time.Sleep(200 * time.Millisecond)
		if err := keyboard.Open(); err != nil {
			// VPS 无 TTY 时不强制恢复，避免卡顿
		}
	}()

	fmt.Println(boxStyle.Render("[ 创建新隧道 ]"))

	tunnel := Tunnel{}

	// [安全] 域名/子域名/端口/名称均做合法性校验，非法输入重新提示，防止路径逃逸与注入
	for {
		fmt.Print(infoStyle.Render("[域名] (如 example.com): "))
		tunnel.Domain, _ = reader.ReadString('\n')
		tunnel.Domain = strings.TrimSpace(tunnel.Domain)
		if isValidDomain(tunnel.Domain) {
			break
		}
		fmt.Println(errorStyle.Render("[X] 域名格式无效（需包含 . 且不含空格/斜杠），请重新输入"))
	}

	for {
		fmt.Print(infoStyle.Render("[子域名] 前缀 (如 api): "))
		tunnel.Subdomain, _ = reader.ReadString('\n')
		tunnel.Subdomain = strings.TrimSpace(tunnel.Subdomain)
		if tunnel.Subdomain == "" {
			tunnel.Subdomain = "app"
		}
		if isValidTunnelName(tunnel.Subdomain) {
			break
		}
		fmt.Println(errorStyle.Render("[X] 子域名仅允许字母/数字/_/- (1-63字符)，请重新输入"))
	}

	defaultPort := "8080"
	if presetPort != "" {
		defaultPort = presetPort
	}
	for {
		fmt.Printf("%s[端口] [%s]: %s", infoStyle.Render(""), defaultPort, ColorReset)
		port, _ := reader.ReadString('\n')
		tunnel.Port = strings.TrimSpace(port)
		if tunnel.Port == "" {
			tunnel.Port = defaultPort
		}
		if p, err := strconv.Atoi(tunnel.Port); err == nil && p > 0 && p <= 65535 {
			break
		}
		fmt.Println(errorStyle.Render("[X] 端口无效（1-65535），请重新输入"))
	}

	// 服务协议：默认保持旧版 http；仅“本机SSH穿透”入口预置为 ssh
	defaultScheme := presetServiceScheme
	if defaultScheme == "" {
		defaultScheme = "http"
	}
	for {
		fmt.Printf("%s[服务协议] http/ssh [%s]: %s", infoStyle.Render(""), defaultScheme, ColorReset)
		scheme, _ := reader.ReadString('\n')
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if scheme == "" {
			scheme = defaultScheme
		}
		if scheme == "http" || scheme == "ssh" || scheme == "https" {
			tunnel.ServiceScheme = scheme
			break
		}
		fmt.Println(errorStyle.Render("[X] 协议仅支持 http/https/ssh，请重新输入"))
	}
	if normalizeServiceScheme(tunnel.ServiceScheme) == "ssh" && tunnel.Port != "22" {
		fmt.Println(warningStyle.Render("[!] 提示: SSH 穿透通常应为 ssh://localhost:22"))
	}

	defaultTunnelName := "mytunnel"
	if presetName != "" {
		defaultTunnelName = presetName
	}
	for {
		fmt.Printf("%s[名称] [%s]: %s", infoStyle.Render(""), defaultTunnelName, ColorReset)
		tunnel.Name, _ = reader.ReadString('\n')
		tunnel.Name = strings.TrimSpace(tunnel.Name)
		if tunnel.Name == "" {
			tunnel.Name = defaultTunnelName
		}
		if isValidTunnelName(tunnel.Name) {
			break
		}
		fmt.Println(errorStyle.Render("[X] 名称仅允许字母/数字/_/- (1-63字符)，请重新输入"))
	}

	fullDomain := tunnel.Subdomain + "." + tunnel.Domain

	existingLocalIndex := -1
	for i, t := range cfg.Tunnels {
		if t.Name == tunnel.Name {
			existingLocalIndex = i
			fmt.Printf("%s[!] 本地已存在同名隧道 '%s'%s\n", ColorYellow, tunnel.Name, ColorReset)
			fmt.Print(infoStyle.Render("是否删除本地配置并重建? [yes/N]: "))
			confirm, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(confirm)) != "yes" {
				fmt.Println(infoStyle.Render("[已取消]"))
				waitForReturn(useKeyboard, reader)
				return
			}

			fmt.Print(infoStyle.Render("[*] 清理旧 DNS 记录... "))
			cleanupDNS(binPath, fullDomain)

			if t.TunnelID != "" {
				os.Remove(filepath.Join(configPath, t.TunnelID+".json"))
			}
			os.Remove(filepath.Join(configPath, t.Name+".yml"))
			break
		}
	}

	// ====== 凭证准备阶段 ======
	if !isLoggedIn(configPath) && !cfg.hasAPICredentials() {
		// 【修改】所有平台统一提示选择授权方式（不再仅按平台判断），
		// 无头 Linux/SSH 环境也能直接使用 API Token
		fmt.Println(boxStyle.Render("[ 需要 Cloudflare 授权 ]"))
		fmt.Println("  [1] 浏览器授权 (cloudflared tunnel login)")
		fmt.Println("  [2] API Token  (无浏览器/无桌面/SSH 环境)")
		fmt.Print(infoStyle.Render("请选择授权方式 [1/2] [默认: 1]: "))
		authChoice, _ := reader.ReadString('\n')
		authChoice = strings.TrimSpace(authChoice)

		if authChoice == "2" {
			if !promptAPICredentials(cfg, reader) {
				fmt.Println(infoStyle.Render("[已取消]"))
				waitForReturn(useKeyboard, reader)
				return
			}
		} else if isBrowserAvailable() {
			// 【分支 A】浏览器授权：cloudflared 输出授权 URL 到终端，用户可自行处理
			fmt.Println(boxStyle.Render(
				"[ 需要 Cloudflare 登录授权 ]\n\n" +
					"[*] 系统将尝试自动打开浏览器，请按页面提示完成登录\n" +
					"[*] 若浏览器未自动弹出，请复制随后显示的链接手动访问\n" +
					"[*] 授权完成后请返回本终端继续操作",
			))
			fmt.Println(ColorYellow + ">>> 正在等待浏览器响应，请前往浏览器操作..." + ColorReset)

			cmd := exec.Command(filepath.Join(binPath, platformInfo.BinaryName), "tunnel", "login")
			if platformInfo.IsDocker {
				cmd = exec.Command("docker", "run", "-it", "--rm",
					"-v", configPath+":/etc/cloudflared",
					"cloudflare/cloudflared:latest", "tunnel", "login")
			}
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Printf("%s[X] 登录命令执行失败: %v%s\n", ColorRed, err, ColorReset)
				fmt.Printf("%s[*] 请检查 cloudflared 二进制是否完整%s\n", ColorBlue, ColorReset)
				waitForReturn(useKeyboard, reader)
				return
			}

			fmt.Println()
			if isLoggedIn(configPath) {
				fmt.Println(successStyle.Render("[OK] 授权成功！凭证已保存"))
			} else {
				fmt.Println(warningStyle.Render("[!] 未检测到凭证，请确认已在浏览器完成授权"))
			}
		} else {
			// 【分支 B】无浏览器环境（Android/iOS/Docker）：引导 API Token
			fmt.Println(warningStyle.Render("[!] 当前环境无法启动浏览器授权"))
			if !promptAPICredentials(cfg, reader) {
				fmt.Println(infoStyle.Render("[已取消]"))
				waitForReturn(useKeyboard, reader)
				return
			}
		}
	}

	// ====== 创建隧道（双模式）======
	var credFile string
	var useAPI bool

	if cfg.hasAPICredentials() {
		// 【API Token 模式】直接调用 Cloudflare REST API
		fmt.Print(infoStyle.Render("[*] 使用 API Token 创建隧道... "))
		if err := createTunnelViaAPI(cfg, &tunnel, configPath); err != nil {
			fmt.Println(errorStyle.Render("[X] " + err.Error()))
			waitForReturn(useKeyboard, reader)
			return
		}
		fmt.Println(successStyle.Render("[OK] 完成"))
		credFile = filepath.Join(configPath, tunnel.TunnelID+".json")
		useAPI = true
	} else {
		// 【浏览器模式】原有命令行创建逻辑完全保留
		fmt.Print(infoStyle.Render("[*] 创建隧道... "))
		cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
		cmd := exec.Command(cloudflaredPath, "tunnel", "create", tunnel.Name)
		if platformInfo.IsDocker {
			cmd = exec.Command("docker", "run", "--rm",
				"-v", configPath+":/etc/cloudflared",
				"cloudflare/cloudflared:latest", "tunnel", "create", tunnel.Name)
		}

		output, err := cmd.CombinedOutput()
		credFile = ""

		if err != nil && strings.Contains(string(output), "already exists") {
			fmt.Println(warningStyle.Render("[!] 云端已存在同名隧道"))
			existingID, err := getTunnelIDByName(binPath, tunnel.Name)
			if err != nil {
				fmt.Printf("%s[X] 无法获取云端隧道ID: %v%s\n", ColorRed, err, ColorReset)
				waitForReturn(useKeyboard, reader)
				return
			}

			fmt.Printf("%s[>] 云端隧道ID: %s%s\n", ColorBlue, existingID, ColorReset)
			fmt.Print(infoStyle.Render("请选择:\n[1] 使用现有\n[2] 删除重建\n[3] 取消\n选择: "))
			choice, _ := reader.ReadString('\n')

			switch strings.TrimSpace(choice) {
			case "1":
				tunnel.TunnelID = existingID
				fmt.Println(successStyle.Render("[OK] 使用现有云端隧道"))
				credFile = filepath.Join(configPath, tunnel.TunnelID+".json")
			case "2":
				fmt.Print(infoStyle.Render("[*] 清理并重建... "))
				cleanupDNS(binPath, fullDomain)
				if err := deleteCloudflareTunnel(binPath, tunnel.Name); err != nil {
					fmt.Println(errorStyle.Render("[X] " + err.Error()))
					waitForReturn(useKeyboard, reader)
					return
				}
				os.Remove(filepath.Join(configPath, existingID+".json"))

				cmd = exec.Command(cloudflaredPath, "tunnel", "create", tunnel.Name)
				if platformInfo.IsDocker {
					cmd = exec.Command("docker", "run", "--rm",
						"-v", configPath+":/etc/cloudflared",
						"cloudflare/cloudflared:latest", "tunnel", "create", tunnel.Name)
				}
				output, err = cmd.CombinedOutput()
				if err != nil {
					fmt.Println(errorStyle.Render("[X] 重建失败"))
					waitForReturn(useKeyboard, reader)
					return
				}

				time.Sleep(2 * time.Second)
				tunnel.TunnelID, _ = getTunnelIDByName(binPath, tunnel.Name)
				credFile = filepath.Join(configPath, tunnel.TunnelID+".json")
				fmt.Println(successStyle.Render("[OK] 重建完成"))
			default:
				fmt.Println(infoStyle.Render("[已取消]"))
				waitForReturn(useKeyboard, reader)
				return
			}
		} else if err != nil {
			fmt.Println(errorStyle.Render("[X] 创建失败: " + string(output)))
			waitForReturn(useKeyboard, reader)
			return
		} else {
			fmt.Println(successStyle.Render("[OK] 完成"))
			time.Sleep(2 * time.Second)
			tunnel.TunnelID, _ = getTunnelIDByName(binPath, tunnel.Name)
			credFile = filepath.Join(configPath, tunnel.TunnelID+".json")
		}

		if credFile == "" {
			fmt.Println(errorStyle.Render("[X] 无法获取凭证路径"))
			waitForReturn(useKeyboard, reader)
			return
		}
	}

	// ====== 配置生成（双模式共用）======
	configFile := filepath.Join(configPath, tunnel.Name+".yml")
	protocol := "quic"
	saveProtocolPreference(tunnel.Name, protocol)

	configContent := fmt.Sprintf(
		`tunnel: %s
credentials-file: %s
protocol: %s

ingress:
  - hostname: %s
    service: %s
  - service: http_status:404
`, tunnel.TunnelID, credFile, protocol, fullDomain, tunnelServiceURL(tunnel))

	os.WriteFile(configFile, []byte(configContent), 0600)
	if !useAPI {
		forceSyncDNS(binPath, tunnel.Name, fullDomain, reader)
	}

	tunnel.Active = true
	if existingLocalIndex >= 0 {
		cfg.Tunnels[existingLocalIndex] = tunnel
	} else {
		cfg.Tunnels = append(cfg.Tunnels, tunnel)
	}

	fmt.Println(boxStyle.Render(
		"[ 隧道创建成功！ ]\n\n" +
			"[URL]  https://" + fullDomain + "\n" +
			"[端口] " + tunnel.Port + "\n" +
			"[协议] " + normalizeServiceScheme(tunnel.ServiceScheme) + "://localhost:" + tunnel.Port + "\n" +
			"[ID]   " + tunnel.TunnelID + "\n" +
			"[配置] " + configFile,
	))

	fmt.Println(infoStyle.Render("\n启动选项:"))
	fmt.Println("  [1] 前台运行（当前终端）")
	fmt.Println("  [2] 后台运行（守护进程）")
	if platformInfo.IsDocker {
		fmt.Println("  [3] Docker 运行")
	}
	fmt.Println("  [4] 暂不启动")
	fmt.Print(infoStyle.Render("选择: "))

	choice, _ := reader.ReadString('\n')
	switch strings.TrimSpace(choice) {
	case "1":
		// 前台模式：内部处理交互，返回后需要回车返回主菜单
		runTunnelDirectly(tunnel, binPath, false, useKeyboard, reader)

		// 前台返回后统一等待回车
		waitForReturn(useKeyboard, reader)

	case "2":
		// VPS 关键修复：后台启动后立即返回，不阻塞键盘，避免卡死
		startDaemon(tunnel, binPath, reader)
		fmt.Println(successStyle.Render("\n[OK] 后台启动完成"))
		fmt.Printf("%s[*] 提示: 使用 [7] 后台管理 查看状态%s\n", ColorBlue, ColorReset)
		time.Sleep(800 * time.Millisecond)
		return

	case "3":
		if platformInfo.IsDocker {
			// Docker 同样是后台，立即返回
			runWithDocker(tunnel)
			fmt.Println(successStyle.Render("\n[OK] Docker 启动完成"))
			time.Sleep(800 * time.Millisecond)
			return
		}
		fallthrough
	default:
		// 选项4(暂不)明确提示等待回车
		fmt.Println(infoStyle.Render("\n[ 配置已保存 ]"))
		waitForReturn(useKeyboard, reader)
	}
}

// cleanupDNS 静默清理指定域名的 DNS 记录
// 用于重建隧道前清理旧记录，避免冲突
func cleanupDNS(binPath, fullDomain string) {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	configPath := filepath.Join(homeDir, ".cloudflared")

	var deleteCmd *exec.Cmd
	if platformInfo.IsDocker {
		deleteCmd = exec.Command("docker", "run", "--rm",
			"-v", configPath+":/etc/cloudflared",
			"cloudflare/cloudflared:latest", "tunnel", "route", "dns", "delete", fullDomain)
	} else {
		deleteCmd = exec.Command(cloudflaredPath, "tunnel", "route", "dns", "delete", fullDomain)
	}

	deleteCmd.Stderr = nil
	deleteCmd.Stdout = nil
	deleteCmd.Run()
	time.Sleep(2 * time.Second)
}

// findTunnelCredentials 查找最新的隧道凭证文件
// 通过轮询等待 cloudflared 创建凭证文件，最多等待 3 秒
func findTunnelCredentials(configPath, name string) (string, string, error) {
	var latestFile os.DirEntry
	var latestTime time.Time

	for i := 0; i < 6; i++ {
		entries, err := os.ReadDir(configPath)
		if err != nil {
			return "", "", err
		}

		latestFile = nil
		latestTime = time.Time{}

		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") && !strings.Contains(entry.Name(), "cert") {
				info, err := entry.Info()
				if err != nil {
					continue
				}
				if info.Size() == 0 {
					continue
				}
				if info.ModTime().After(latestTime) {
					latestTime = info.ModTime()
					latestFile = entry
				}
			}
		}

		if latestFile != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if latestFile == nil {
		return "", "", fmt.Errorf("未找到凭证文件")
	}

	tunnelID := strings.TrimSuffix(latestFile.Name(), ".json")
	return tunnelID, filepath.Join(configPath, latestFile.Name()), nil
}

// manageTunnels 隧道管理菜单
// 修复：优化 keyboard 的开关时机，避免与 reader 冲突
// [修改] 添加协议切换功能 (P键)，支持动态切换 http2/quic/auto
func manageTunnels(cfg *Config, configPath, binPath string, useKeyboard bool, reader *bufio.Reader) {
	if len(cfg.Tunnels) == 0 {
		fmt.Println(warningStyle.Render("[!] 没有隧道可管理"))
		time.Sleep(1 * time.Second)
		return
	}

	// 不在函数开头打开 keyboard，而是在循环内按需打开
	selected := 1

	for {
		clearScreen()
		width := 80
		if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
			if w, _, err := term.GetSize(fd); err == nil {
				width = w
			}
		}

		drawCenteredTitleBox(ColorCyan+"[ 隧道管理 ]"+ColorReset, width)

		if !useKeyboard {
			fmt.Printf("%s[数字模式] 输入数字选择隧道，字母执行操作%s\n\n", ColorYellow, ColorReset)
		}

		currentTunnel := cfg.Tunnels[selected-1]

		// [修改] 显示当前协议
		currentProto := getProtocolForTunnel(currentTunnel.Name)

		fmt.Printf("%s当前选中: %s%s%s (%s.%s:%s) 协议:%s%s%s\n\n",
			ColorYellow, ColorCyan, currentTunnel.Name, ColorReset,
			currentTunnel.Subdomain, currentTunnel.Domain, currentTunnel.Port,
			ColorBlue, currentProto, ColorReset)

		for i, t := range cfg.Tunnels {
			statusStr, statusColor := getTunnelStatusDisplay(t.Name)

			// 添加 Docker 状态
			if isDockerRunning(t.Name) {
				statusStr = "[Docker]"
				statusColor = ColorBlue
			}

			prefix := "   "
			lineColor := ColorReset
			if i == selected-1 {
				prefix = " > "
				lineColor = ColorCyan
			}

			// 显示协议信息
			proto := getProtocolForTunnel(t.Name)

			fmt.Printf("%s%s%2d. %-15s -> https://%s.%s:%-5s [%s] %s%s%s\n",
				prefix, lineColor, i+1, t.Name, t.Subdomain, t.Domain, t.Port,
				proto, statusColor, statusStr, ColorReset)
		}

		// [修改] 添加 P[协议] 选项
		fmt.Printf("\n%s操作: [数字]选择 %sS[启动] K[停止] B[后台] P[协议] E[编辑] X[删除] A[默认] Q[返回]%s\n",
			ColorYellow, ColorGreen, ColorReset)
		fmt.Print(infoStyle.Render("\n请输入: "))

		var cmd string

		if !useKeyboard {
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if len(input) > 0 {
				cmd = string(input[0])
				if num, err := strconv.Atoi(input); err == nil && num >= 1 && num <= len(cfg.Tunnels) {
					selected = num
					continue
				}
			}
		} else {
			// 需要读取键盘前打开
			if err := keyboard.Open(); err != nil {
				useKeyboard = false
				continue
			}

			char, key, err := keyboard.GetKey()
			keyboard.Close() // 立即关闭，释放资源

			if err != nil {
				continue
			}

			switch key {
			case keyboard.KeyArrowUp:
				selected--
				if selected < 1 {
					selected = len(cfg.Tunnels)
				}
				continue
			case keyboard.KeyArrowDown:
				selected++
				if selected > len(cfg.Tunnels) {
					selected = 1
				}
				continue
			case keyboard.KeyEsc:
				return
			default:
				cmd = strings.ToUpper(string(char))
			}
		}

		// 执行操作前确保 keyboard 关闭
		keyboard.Close()

		switch cmd {
		case "Q":
			return
		case "S":
			_ = runTunnelDirectly(cfg.Tunnels[selected-1], binPath, false, useKeyboard, reader)
			resetTerminal()
			time.Sleep(200 * time.Millisecond)

		case "K":
			stopTunnel(cfg.Tunnels[selected-1].Name)
			stopDaemon(cfg.Tunnels[selected-1].Name)
			stopDockerTunnel(cfg.Tunnels[selected-1].Name)
			fmt.Println(successStyle.Render("\n[OK] 已停止"))
			time.Sleep(1 * time.Second)

		case "B":
			startDaemon(cfg.Tunnels[selected-1], binPath, reader)
			time.Sleep(1 * time.Second)

		case "D":
			if platformInfo.IsDocker {
				runWithDocker(cfg.Tunnels[selected-1])
			} else {
				fmt.Println(warningStyle.Render("\n[!] 当前非 Docker 环境"))
				time.Sleep(1 * time.Second)
			}

		// [新增] 协议切换功能
		case "P":
			currentProto := getProtocolForTunnel(currentTunnel.Name)
			fmt.Printf("\n%s当前隧道: %s%s%s\n", ColorCyan, ColorYellow, currentTunnel.Name, ColorReset)
			fmt.Printf("%s当前协议: %s%s%s\n\n", ColorCyan, ColorBlue, currentProto, ColorReset)

			fmt.Println(ColorYellow + "可用协议:" + ColorReset)
			fmt.Println("  [1] quic  (UDP协议，抗干扰能力强，推荐)")
			fmt.Println("  [2] http2 (TCP协议，稳定性好)")
			fmt.Println("  [3] auto  (自动选择)")
			fmt.Println("  [4] 取消")
			fmt.Print(infoStyle.Render("\n选择新协议 [1-4]: "))

			// 临时关闭 keyboard 使用 reader 读取选择
			if useKeyboard {
				keyboard.Close()
			}

			choice, _ := reader.ReadString('\n')
			choice = strings.TrimSpace(choice)
			newProto := ""

			switch choice {
			case "1":
				newProto = "quic"
			case "2":
				newProto = "http2"
			case "3":
				newProto = "auto"
			case "4", "":
				fmt.Println(infoStyle.Render("[已取消]"))
				time.Sleep(500 * time.Millisecond)
				continue
			default:
				fmt.Println(warningStyle.Render("[!] 无效选择"))
				time.Sleep(500 * time.Millisecond)
				continue
			}

			if newProto != "" {
				// 保存协议偏好
				saveProtocolPreference(currentTunnel.Name, newProto)

				// 更新配置文件
				configFile := filepath.Join(configPath, currentTunnel.Name+".yml")
				updateConfigProtocol(configFile, newProto)

				fmt.Printf("%s[OK] 已切换协议: %s -> %s%s\n",
					ColorGreen, currentProto, newProto, ColorReset)
				fmt.Printf("%s[*] 下次启动时生效%s\n", ColorBlue, ColorReset)

				// 如果隧道正在运行，询问是否重启
				if isTunnelRunning(currentTunnel.Name) || getDaemonPID(currentTunnel.Name) > 0 {
					fmt.Print(infoStyle.Render("\n隧道正在运行，是否立即重启应用新协议? [y/N]: "))
					restart, _ := reader.ReadString('\n')
					if strings.TrimSpace(strings.ToLower(restart)) == "y" {
						fmt.Printf("%s[*] 正在重启隧道...%s\n", ColorYellow, ColorReset)

						// 停止隧道
						stopTunnel(currentTunnel.Name)
						stopDaemon(currentTunnel.Name)
						stopDockerTunnel(currentTunnel.Name)
						time.Sleep(500 * time.Millisecond)

						// 重新启动 - 传入完整的 Tunnel 结构体，而不是仅传名称
						startDaemon(currentTunnel, binPath, reader)
					}
				}

				time.Sleep(1 * time.Second)
			}

		case "E":
			editTunnel(&cfg.Tunnels[selected-1], reader)
			saveConfig(*cfg, filepath.Join(homeDir, saveFile))
			time.Sleep(500 * time.Millisecond)

		case "X":
			deleteTunnel(cfg, selected-1, configPath)
			if len(cfg.Tunnels) == 0 {
				fmt.Println(infoStyle.Render("\n[!] 所有隧道已删除，返回主菜单..."))
				time.Sleep(1 * time.Second)
				return
			}
			if selected > len(cfg.Tunnels) {
				selected = len(cfg.Tunnels)
			}
			time.Sleep(500 * time.Millisecond)

		case "A":
			for i := range cfg.Tunnels {
				cfg.Tunnels[i].Active = false
			}
			cfg.Tunnels[selected-1].Active = true
			fmt.Println(successStyle.Render("\n[OK] 已设为默认"))
			time.Sleep(1 * time.Second)
		}
	}
}

// isDockerRunning 检查 Docker 容器是否正在运行
func isDockerRunning(name string) bool {
	cmd := exec.Command("docker", "ps", "-q", "-f", "name=cf-tunnel-"+name)
	output, _ := cmd.Output()
	return len(output) > 0
}

// switchTunnel 切换并启动隧道菜单（无 Emoji 版）
// 快速选择并启动其他隧道，支持前台、后台、Docker 三种模式
func switchTunnel(cfg *Config, configPath, binPath string, useKeyboard bool, reader *bufio.Reader) {
	if len(cfg.Tunnels) == 0 {
		fmt.Println(warningStyle.Render("[!] 没有可用隧道"))
		time.Sleep(1 * time.Second)
		return
	}

	if useKeyboard {
		if err := keyboard.Open(); err != nil {
			useKeyboard = false
		} else {
			defer keyboard.Close()
		}
	}

	selected := 1
	for {
		clearScreen()
		width := 80
		if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
			if w, _, err := term.GetSize(fd); err == nil {
				width = w
			}
		}
		drawCenteredTitleBox(ColorCyan+"[ 切换隧道 ]"+ColorReset, width)

		if !useKeyboard {
			fmt.Printf("%s[数字]选择 [S]启动 [B]后台 [D]Docker [Q]返回%s\n\n", ColorYellow, ColorReset)
		}

		for i, t := range cfg.Tunnels {
			status := "[.]"
			if getDaemonPID(t.Name) > 0 {
				status = "[BG]"
			} else if isTunnelRunning(t.Name) {
				status = "[FG]"
			} else if isDockerRunning(t.Name) {
				status = "[Docker]"
			}

			prefix := "   "
			lineColor := ColorReset
			if i+1 == selected {
				prefix = " > "
				lineColor = ColorCyan
			}

			fmt.Printf("%s%s%2d. %s %-15s -> https://%s.%s%s\n",
				prefix, lineColor, i+1, status, t.Name, t.Subdomain, t.Domain, ColorReset)
		}

		fmt.Print(infoStyle.Render("\n请输入: "))

		var cmd string

		if !useKeyboard {
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if num, err := strconv.Atoi(input); err == nil && num >= 1 && num <= len(cfg.Tunnels) {
				selected = num
				continue
			}
			if len(input) > 0 {
				cmd = string(input[0])
			}
		} else {
			char, key, err := keyboard.GetKey()
			if err != nil {
				continue
			}
			switch key {
			case keyboard.KeyArrowUp:
				selected--
				if selected < 1 {
					selected = len(cfg.Tunnels)
				}
				continue
			case keyboard.KeyArrowDown:
				selected++
				if selected > len(cfg.Tunnels) {
					selected = 1
				}
				continue
			case keyboard.KeyEsc:
				return
			default:
				cmd = strings.ToUpper(string(char))
			}
		}

		switch cmd {
		case "Q":
			return
		case "S":
			for i := range cfg.Tunnels {
				cfg.Tunnels[i].Active = false
			}
			cfg.Tunnels[selected-1].Active = true

			if useKeyboard {
				keyboard.Close()
			}
			runTunnelDirectly(cfg.Tunnels[selected-1], binPath, false, useKeyboard, reader)

			// [关键修复] 重置终端
			resetTerminal()

			if useKeyboard {
				for i := 0; i < 3; i++ {
					if err := keyboard.Open(); err == nil {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
			}
			return
		case "B":
			for i := range cfg.Tunnels {
				cfg.Tunnels[i].Active = false
			}
			cfg.Tunnels[selected-1].Active = true
			keyboard.Close() // 后台启动内部可能读取按键（跳过等待），先释放键盘
			startDaemon(cfg.Tunnels[selected-1], binPath, reader)
			return
		case "D":
			for i := range cfg.Tunnels {
				cfg.Tunnels[i].Active = false
			}
			cfg.Tunnels[selected-1].Active = true
			runWithDocker(cfg.Tunnels[selected-1])
			return
		}
	}
}

// ====== 后台管理（无 Emoji 版）======
// daemonManager 后台隧道管理菜单（无 Emoji 版）
// 查看运行中的后台进程和 Docker 容器，支持查看日志、终止、重启操作
func daemonManager(cfg Config, binPath string, useKeyboard bool, reader *bufio.Reader) {
	if useKeyboard {
		if err := keyboard.Open(); err != nil {
			useKeyboard = false
		} else {
			defer keyboard.Close()
		}
	}

	for {
		clearScreen()
		width := 80
		if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
			if w, _, err := term.GetSize(fd); err == nil {
				width = w
			}
		}
		drawCenteredTitleBox(ColorCyan+"[ 后台隧道管理 ]"+ColorReset, width)

		running := listRunningDaemons(cfg)
		dockerRunning := listDockerRunning(cfg)

		if len(running) == 0 && len(dockerRunning) == 0 {
			fmt.Println(warningStyle.Render("  [!] 没有后台运行的隧道"))
		} else {
			if len(running) > 0 {
				fmt.Println(infoStyle.Render("  [本地进程]"))
				for _, t := range running {
					status := checkTunnelRealStatus(t.Name)
					statusIcon := "?"
					statusColor := ColorYellow

					if status.Connected {
						statusIcon = "✓"
						statusColor = ColorGreen
					} else if status.LastError != "" {
						statusIcon = "✗"
						statusColor = ColorRed
					} else if status.Running {
						statusIcon = "..."
						statusColor = ColorYellow
					}

					// [修复] 简化输出，避免复杂的格式化
					protoStr := status.Protocol
					if protoStr == "" {
						protoStr = "unknown"
					}

					fmt.Printf("    %s%s%-15s PID:%d [%s] %s%s\n",
						statusColor, statusIcon, t.Name, getDaemonPID(t.Name),
						protoStr, getLogPath(t.Name), ColorReset)

					if status.LastError != "" {
						fmt.Printf("        %s错误: %s%s\n", ColorRed, status.LastError, ColorReset)
					}
				}
			}
			if len(dockerRunning) > 0 {
				fmt.Println(infoStyle.Render("  [Docker 容器]"))
				for _, t := range dockerRunning {
					fmt.Printf("    [D] %-15s 容器:cf-tunnel-%s\n", t.Name, t.Name)
				}
			}
		}

		fmt.Println("\n" + ColorYellow + "操作选项:" + ColorReset)
		fmt.Println("  [V] 查看日志")
		fmt.Println("  [K] 终止指定隧道")
		fmt.Println("  [A] 终止全部隧道")
		fmt.Println("  [R] 重启隧道")
		fmt.Println("  [Q] 返回上级菜单")
		fmt.Print(infoStyle.Render("\n请选择: "))

		var cmd string

		if !useKeyboard {
			input, _ := reader.ReadString('\n')
			if len(input) > 0 {
				cmd = strings.ToUpper(string(input[0]))
			}
		} else {
			char, key, err := keyboard.GetKey()
			if err != nil {
				continue
			}
			if key == keyboard.KeyEsc {
				return
			}
			cmd = strings.ToUpper(string(char))
		}

		switch cmd {
		case "Q":
			return
		case "V":
			viewLogMenu(cfg, width, useKeyboard, reader)
		case "K":
			// VPS 修复：临时关闭 keyboard 避免读取冲突
			if useKeyboard {
				keyboard.Close()
			}
			fmt.Print(infoStyle.Render("输入隧道名称终止: "))
			name, _ := reader.ReadString('\n')
			name = strings.TrimSpace(name)
			stopped := false
			if stopDaemon(name) {
				stopped = true
			}
			stopDockerTunnel(name)
			if stopped {
				fmt.Println(successStyle.Render("[OK] 已终止: " + name))
			} else {
				fmt.Println(errorStyle.Render("[X] 未找到: " + name))
			}
			time.Sleep(1 * time.Second)
			// 重新打开 keyboard
			if useKeyboard {
				if err := keyboard.Open(); err != nil {
					useKeyboard = false
				}
			}
		case "A":
			// VPS 修复：临时关闭 keyboard 避免读取冲突
			if useKeyboard {
				keyboard.Close()
			}
			fmt.Print(warningStyle.Render("[!] 确定终止全部? [yes/N]: "))
			confirm, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(confirm)) == "yes" {
				for _, t := range cfg.Tunnels {
					stopDaemon(t.Name)
					stopDockerTunnel(t.Name)
				}
				fmt.Println(successStyle.Render("[OK] 已全部终止"))
			}
			time.Sleep(1 * time.Second)
			// 重新打开 keyboard
			if useKeyboard {
				if err := keyboard.Open(); err != nil {
					useKeyboard = false
				}
			}
		case "R":
			// VPS 修复：临时关闭 keyboard 避免读取冲突
			if useKeyboard {
				keyboard.Close()
			}
			fmt.Print(infoStyle.Render("输入隧道名称重启: "))
			name, _ := reader.ReadString('\n')
			name = strings.TrimSpace(name)
			for _, t := range cfg.Tunnels {
				if t.Name == name {
					stopDaemon(name)
					stopDockerTunnel(name)
					time.Sleep(500 * time.Millisecond)
					startDaemon(t, binPath, reader)
					fmt.Println(successStyle.Render("[OK] 已重启"))
					break
				}
			}
			time.Sleep(1 * time.Second)
			// 重新打开 keyboard
			if useKeyboard {
				if err := keyboard.Open(); err != nil {
					useKeyboard = false
				}
			}
		}
	}
}

// viewLogMenu 日志查看菜单（无 Emoji 版）
// 列出所有隧道的日志文件，支持查看和选择
func viewLogMenu(cfg Config, width int, useKeyboard bool, reader *bufio.Reader) {
	if len(cfg.Tunnels) == 0 {
		fmt.Println(warningStyle.Render("\n[!] 没有可查看的隧道配置"))
		time.Sleep(1 * time.Second)
		return
	}

	selected := 1
	for {
		clearScreen()
		drawCenteredTitleBox(ColorCyan+"[ 选择要查看的日志 ]"+ColorReset, width)

		if !useKeyboard {
			fmt.Printf("%s[数字]选择 [Enter]查看 [Q]返回%s\n\n", ColorYellow, ColorReset)
		}

		for i, t := range cfg.Tunnels {
			status := "[STOP]"
			if getDaemonPID(t.Name) > 0 {
				status = "[RUNNING]"
			} else if isDockerRunning(t.Name) {
				status = "[DOCKER]"
			}

			logPath := getLogPath(t.Name)
			logInfo := ""
			if info, err := os.Stat(logPath); err == nil {
				logInfo = fmt.Sprintf("(%.1fKB)", float64(info.Size())/1024)
			} else {
				logInfo = "(无日志)"
			}

			prefix := "   "
			lineColor := ColorReset
			if i+1 == selected {
				prefix = " > "
				lineColor = ColorCyan
			}
			fmt.Printf("%s%s%2d. %-15s %s %s%s\n",
				prefix, lineColor, i+1, t.Name, status, logInfo, ColorReset)
		}

		fmt.Print(infoStyle.Render("\n请选择: "))

		if !useKeyboard {
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if input == "q" {
				return
			}
			if num, err := strconv.Atoi(input); err == nil && num >= 1 && num <= len(cfg.Tunnels) {
				selected = num
			} else if input == "" {
				viewLogSafe(cfg.Tunnels[selected-1].Name, useKeyboard, reader)
			}
		} else {
			char, key, err := keyboard.GetKey()
			if err != nil {
				continue
			}
			switch key {
			case keyboard.KeyArrowUp:
				selected--
				if selected < 1 {
					selected = len(cfg.Tunnels)
				}
			case keyboard.KeyArrowDown:
				selected++
				if selected > len(cfg.Tunnels) {
					selected = 1
				}
			case keyboard.KeyEnter:
				viewLogSafe(cfg.Tunnels[selected-1].Name, useKeyboard, reader)
			case keyboard.KeyEsc:
				return
			default:
				if char == 'q' || char == 'Q' {
					return
				}
			}
		}
	}
}

// viewLogSafe 安全地查看指定隧道的日志
// 显示最后 100 行日志，支持键盘和数字两种交互模式
func viewLogSafe(name string, useKeyboard bool, reader *bufio.Reader) {
	logPath := getLogPath(name)

	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Printf("\n%s[!] %s 的日志文件不存在%s\n", ColorYellow, name, ColorReset)
		fmt.Printf("%s[按回车返回...]%s", ColorYellow, ColorReset)
		if useKeyboard {
			keyboard.GetKey()
		} else {
			reader.ReadString('\n')
		}
		return
	}

	fmt.Printf("\n%s=== %s 的日志（最后100行）===%s\n", ColorYellow, name, ColorReset)

	if runtime.GOOS == "android" {
		fmt.Printf("%s[*] Android 日志路径: %s%s\n", ColorCyan, logPath, ColorReset)
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-Command",
			fmt.Sprintf("Get-Content -Path '%s' -Tail 100", logPath))
	} else {
		cmd = exec.Command("tail", "-n", "100", logPath)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	fmt.Printf("\n%s[按回车返回...]%s", ColorYellow, ColorReset)
	if useKeyboard {
		keyboard.GetKey()
	} else {
		reader.ReadString('\n')
	}
}

// listDockerRunning 列出正在 Docker 中运行的隧道
func listDockerRunning(cfg Config) []Tunnel {
	var running []Tunnel
	for _, t := range cfg.Tunnels {
		if isDockerRunning(t.Name) {
			running = append(running, t)
		}
	}
	return running
}

// waitSkippable 倒计时等待（如 DNS 传播），期间按任意键/回车可跳过
// 键盘模式用 keyboard 监听；非键盘模式无法安全并发读 stdin，退化为纯倒计时
func waitSkippable(reader *bufio.Reader, seconds int) {
	fmt.Printf("%s[*] 等待 %d 秒（按回车跳过）: %s", ColorYellow, seconds, ColorReset)

	keyCh := make(chan struct{}, 1)
	keyboardOK := keyboard.Open() == nil
	if keyboardOK {
		defer keyboard.Close()
		go func() {
			// keyboard.Close() 会让阻塞中的 GetKey 返回错误，保证 goroutine 能退出
			_, _, err := keyboard.GetKey()
			if err == nil {
				keyCh <- struct{}{}
			}
		}()
	}

	timeout := time.After(time.Duration(seconds) * time.Second)
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for i := seconds; i > 0; {
		select {
		case <-keyCh:
			fmt.Println()
			fmt.Printf("%s[*] 已跳过等待%s\n", ColorBlue, ColorReset)
			return
		case <-timeout:
			fmt.Println()
			return
		case <-tick.C:
			i--
			fmt.Printf("%d...", i)
		}
	}
	fmt.Println()
}

// forceSyncDNS 强制同步 DNS 记录
// 尝试自动绑定域名到隧道，处理已存在记录的情况，失败时提供手动配置指导
// [修改] 等待 DNS 传播的 30 秒固定 sleep 改为可跳过的倒计时；识别限流(429/81044)并退避重试
func forceSyncDNS(binPath, tunnelName, fullDomain string, reader *bufio.Reader) error {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	configPath := filepath.Join(homeDir, ".cloudflared")

	fmt.Printf("%s[*] 正在同步 DNS 记录: %s -> %s...%s\n", ColorBlue, fullDomain, tunnelName, ColorReset)

	tunnelID, _ := getTunnelIDByName(binPath, tunnelName)

	parts := strings.Split(fullDomain, ".")
	if len(parts) < 2 {
		return fmt.Errorf("无效的域名格式: %s", fullDomain)
	}
	subdomain := parts[0]
	domain := strings.Join(parts[1:], ".")

	for _, useOverwrite := range []bool{false, true} {
		args := []string{"tunnel", "route", "dns"}
		if useOverwrite {
			args = append(args, "--overwrite-dns")
			fmt.Printf("%s[*] 尝试强制覆盖模式...%s\n", ColorYellow, ColorReset)
		}
		args = append(args, tunnelName, fullDomain)

		var output []byte
		var err error
		// [新增] 限流退避：429 / 81044 时等待 15 秒后重试一次
		for try := 0; try < 2; try++ {
			var cmd *exec.Cmd
			if platformInfo.IsDocker {
				dockerArgs := []string{"run", "--rm", "-v", configPath + ":/etc/cloudflared",
					"cloudflare/cloudflared:latest"}
				dockerArgs = append(dockerArgs, args...)
				cmd = exec.Command("docker", dockerArgs...)
			} else {
				cmd = exec.Command(cloudflaredPath, args...)
			}

			output, err = cmd.CombinedOutput()
			if err == nil {
				break
			}
			outStr := string(output)
			if try == 0 && (strings.Contains(outStr, "429 Too Many") ||
				strings.Contains(outStr, "code: 429") ||
				strings.Contains(outStr, "81044")) {
				fmt.Printf("%s[!] 触发 Cloudflare 限流(429)，等待 15 秒后重试...%s\n", ColorYellow, ColorReset)
				time.Sleep(15 * time.Second)
				continue
			}
			break
		}
		if err == nil {
			fmt.Printf("%s[OK] DNS 同步成功%s\n", ColorGreen, ColorReset)
			return nil
		}

		outputStr := string(output)
		if !strings.Contains(outputStr, "already exists") &&
			!strings.Contains(outputStr, "code: 1003") &&
			!strings.Contains(outputStr, "record with that host already exists") {
			return fmt.Errorf("%s", outputStr)
		}
	}

	fmt.Printf("%s[!] 尝试清理并重试...%s\n", ColorYellow, ColorReset)

	var deleteCmd *exec.Cmd
	if platformInfo.IsDocker {
		deleteCmd = exec.Command("docker", "run", "--rm",
			"-v", configPath+":/etc/cloudflared",
			"cloudflare/cloudflared:latest", "tunnel", "route", "dns", "delete", fullDomain)
	} else {
		deleteCmd = exec.Command(cloudflaredPath, "tunnel", "route", "dns", "delete", fullDomain)
	}
	deleteCmd.Run()

	// [修改] 等待 DNS 传播：30 秒可跳过倒计时，替代原来的固定 sleep
	waitSkippable(reader, 30)

	var finalCmd *exec.Cmd
	if platformInfo.IsDocker {
		finalCmd = exec.Command("docker", "run", "--rm",
			"-v", configPath+":/etc/cloudflared",
			"cloudflare/cloudflared:latest", "tunnel", "route", "dns", tunnelName, fullDomain)
	} else {
		finalCmd = exec.Command(cloudflaredPath, "tunnel", "route", "dns", tunnelName, fullDomain)
	}

	_, err := finalCmd.CombinedOutput()
	if err == nil {
		fmt.Printf("%s[OK] DNS 同步成功%s\n", ColorGreen, ColorReset)
		return nil
	}

	fmt.Printf("%s[X] 自动绑定失败%s\n", ColorRed, ColorReset)
	fmt.Printf("%s========================================%s\n", ColorYellow, ColorReset)
	fmt.Printf("%s请手动添加 DNS 记录：%s\n", ColorCyan, ColorReset)
	fmt.Printf("方法 A（Dashboard）：\n")
	fmt.Printf("  1. 登录 https://dash.cloudflare.com\n")
	fmt.Printf("  2. 选择域名 %s -> DNS\n", domain)
	fmt.Printf("  3. 删除现有的 %s 记录（如果有）\n", fullDomain)
	fmt.Printf("  4. 添加新记录：\n")
	fmt.Printf("     类型：CNAME\n")
	fmt.Printf("     名称：%s\n", subdomain)
	if tunnelID != "" {
		fmt.Printf("     目标：%s.cfargotunnel.com\n", tunnelID)
	} else {
		fmt.Printf("     目标：<隧道ID>.cfargotunnel.com\n")
	}
	fmt.Printf("     代理状态：已代理（橙色云）\n")
	fmt.Printf("方法 B（命令行）：\n")
	fmt.Printf("  cloudflared tunnel route dns delete %s\n", fullDomain)
	fmt.Printf("  cloudflared tunnel route dns %s %s\n", tunnelName, fullDomain)
	fmt.Printf("%s========================================%s\n", ColorYellow, ColorReset)

	return fmt.Errorf("DNS 绑定失败：记录 %s 已被占用", fullDomain)
}

// startDaemon 以后台守护进程方式启动隧道
// [修改] 支持协议自动检测和切换，使用真实连接状态验证（而非仅进程存在）
func startDaemon(tunnel Tunnel, binPath string, reader *bufio.Reader) bool {
	stopDaemon(tunnel.Name)

	fullDomain := tunnel.Subdomain + "." + tunnel.Domain
	if err := forceSyncDNS(binPath, tunnel.Name, fullDomain, reader); err != nil {
		fmt.Printf("%s[!] DNS 同步警告: %v%s\n", ColorYellow, err, ColorReset)
	}

	logPath := getLogPath(tunnel.Name)
	pidPath := getPIDPath(tunnel.Name)
	configFile := filepath.Join(homeDir, ".cloudflared", tunnel.Name+".yml")

	// 清理旧日志
	if _, err := os.Stat(logPath); err == nil {
		os.Remove(logPath)
	}

	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		fmt.Println(errorStyle.Render("[X] 配置文件不存在: " + configFile))
		return false
	}

	// [修改] 尝试多个协议启动
	protocols := []string{
		getProtocolForTunnel(tunnel.Name), // 用户偏好或上次成功的
		"quic",                            // 备选1：UDP协议，抗干扰
		"http2",                           // 备选2：TCP协议
		"auto",                            // 最后尝试自动选择
	}

	// 去重
	seen := make(map[string]bool)
	uniqueProtocols := []string{}
	for _, p := range protocols {
		if !seen[p] {
			seen[p] = true
			uniqueProtocols = append(uniqueProtocols, p)
		}
	}

	for i, protocol := range uniqueProtocols {
		if i > 0 {
			fmt.Printf("%s[!] 协议 %s 连接失败，尝试 %s...%s\n",
				ColorYellow, uniqueProtocols[i-1], protocol, ColorReset)
			// 更新配置文件协议
			updateConfigProtocol(configFile, protocol)
			saveProtocolPreference(tunnel.Name, protocol)
		} else {
			fmt.Printf("%s[*] 使用协议: %s%s\n", ColorBlue, protocol, ColorReset)
			// 确保配置文件协议正确
			updateConfigProtocol(configFile, protocol)
		}

		// [修改] 使用新的启动函数，内部包含真实状态验证
		if success := tryStartDaemonWithVerify(tunnel, binPath, logPath, pidPath, configFile); success {
			if i > 0 {
				fmt.Printf("%s[OK] 协议 %s 启动成功！%s\n",
					ColorGreen, protocol, ColorReset)
			}
			// [新增] 显示最终状态
			status := checkTunnelRealStatus(tunnel.Name)
			fmt.Printf("%s[OK] 隧道已连接 (协议: %s, PID: %d)%s\n",
				ColorGreen, status.Protocol, getDaemonPID(tunnel.Name), ColorReset)
			return true
		}

		// [修改] 获取详细错误信息
		status := checkTunnelRealStatus(tunnel.Name)
		if status.LastError != "" {
			fmt.Printf("%s[X] 错误: %s%s\n", ColorRed, status.LastError, ColorReset)
		}

		// 检查是否是协议被阻断，如果不是则停止重试
		if !isProtocolBlockedError(logPath) && status.LastError != "" {
			fmt.Printf("%s[X] 启动失败（非网络问题），停止重试%s\n", ColorRed, ColorReset)
			return false
		}

		// 清理日志和PID文件以便下次判断
		os.Remove(logPath)
		os.Remove(pidPath)
	}

	fmt.Println(errorStyle.Render("[X] 所有协议均无法连接，请检查网络"))
	return false
}

// tryStartDaemonWithVerify 尝试启动守护进程并验证真实连接状态
// [新增] 替换原来的 tryStartDaemonOnce，使用准确的状态检测
func tryStartDaemonWithVerify(tunnel Tunnel, binPath, logPath, pidPath, configFile string) bool {
	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	args := []string{
		"tunnel",
		"--config", configFile,
		"run",
		tunnel.Name,
	}

	cmd := exec.Command(cloudflaredPath, args...)

	// 打开日志文件
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return false
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	procMgr.SetupDaemonProcess(cmd)

	// 启动进程
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return false
	}

	// 立即保存PID（用于后续状态检查）
	os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	// [关键修改] 等待真实连接成功，而不仅是进程启动
	fmt.Printf("%s[*] 等待连接...", ColorYellow)

	// 最多等待15秒
	timeout := time.After(15 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	connected := false

	for {
		select {
		case <-timeout:
			goto CHECK_RESULT
		case <-ticker.C:
			// 检查进程是否还在运行
			if !isProcessRunning(cmd.Process.Pid) {
				fmt.Printf(" %s[进程退出]%s\n", ColorRed, ColorReset)
				logFile.Close()
				return false
			}

			// 检查是否已连接
			status := checkTunnelRealStatus(tunnel.Name)
			if status.Connected {
				fmt.Printf(" %s[已连接]%s\n", ColorGreen, ColorReset)
				connected = true
				goto CHECK_RESULT
			}

			// 检查是否有错误
			if status.LastError != "" {
				fmt.Printf(" %s[错误]%s\n", ColorRed, ColorReset)
				goto CHECK_RESULT
			}

			fmt.Print(".")
		}
	}

CHECK_RESULT:
	logFile.Close()

	if !connected {
		// 终止失败的进程
		procMgr.KillProcess(cmd.Process.Pid, false)
		time.Sleep(200 * time.Millisecond)
		if isProcessRunning(cmd.Process.Pid) {
			procMgr.KillProcess(cmd.Process.Pid, true)
		}
		os.Remove(pidPath)
		return false
	}

	// 成功，保持进程运行
	return true
}

func isProcessRunning(pid int) bool {
	return procMgr.IsProcessRunning(pid)
}

// stopDaemon 停止指定名称的后台守护进程
func stopDaemon(name string) bool {
	pid := getDaemonPID(name)
	if pid <= 0 {
		return false
	}
	procMgr.KillProcessGroup(pid, false)
	time.Sleep(500 * time.Millisecond)
	if isProcessRunning(pid) {
		procMgr.KillProcessGroup(pid, true)
		time.Sleep(100 * time.Millisecond)
	}
	os.Remove(getPIDPath(name))
	return true
}

// getDaemonPID 从 PID 文件读取进程 ID 并验证进程是否存在
// 如果进程不存在则自动清理过期的 PID 文件
// [安全] 若 PID 存活但已不是 cloudflared（PID 复用），同样视为过期并删除，绝不向无关进程发信号
func getDaemonPID(name string) int {
	pidPath := getPIDPath(name)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if !isProcessRunning(pid) {
		os.Remove(pidPath)
		return 0
	}
	if !procMgr.IsCloudflaredProcess(pid) {
		fmt.Printf("%s[!] PID 文件 %d 对应的进程不是 cloudflared（可能已复用），已忽略%s\n",
			ColorYellow, pid, ColorReset)
		os.Remove(pidPath)
		return 0
	}
	return pid
}

// checkTunnelConnected 检查日志中是否包含成功连接的标志
func checkTunnelConnected(logPath string) bool {
	content := readLogTail(logPath, 64<<10)
	if content == "" {
		return false
	}
	return strings.Contains(content, "Registered tunnel connection") ||
		strings.Contains(content, "INF Connected") ||
		strings.Contains(content, "Starting metrics server")
}

// checkTunnelError 检查日志中是否包含致命错误
func checkTunnelError(logPath string) bool {
	content := readLogTail(logPath, 64<<10)
	if content == "" {
		return false
	}
	errorPatterns := []string{
		"ERR Failed to",
		"Authentication error",
		"Cannot establish connection",
		"Invalid tunnel",
		"credentials not found",
		"failed to unmarshal",
	}
	for _, pattern := range errorPatterns {
		if strings.Contains(content, pattern) {
			return true
		}
	}
	return false
}

// isTunnelHealthy 检查隧道健康状态
// 综合检查进程存在性和日志更新时间（30秒内是否有更新）
func isTunnelHealthy(name string) bool {
	logPath := getLogPath(name)

	pid := getDaemonPID(name)
	if pid <= 0 {
		return false
	}

	info, err := os.Stat(logPath)
	if err != nil {
		return false
	}

	if time.Since(info.ModTime()) > 30*time.Second {
		return false
	}

	return true
}

// listRunningDaemons 列出所有正在运行的后台隧道
func listRunningDaemons(cfg Config) []Tunnel {
	var running []Tunnel
	for _, t := range cfg.Tunnels {
		if getDaemonPID(t.Name) > 0 {
			running = append(running, t)
		}
	}
	return running
}

// getLogPath 获取指定隧道日志文件的完整路径
// [安全] 用 filepath.Base 剥离路径分隔符，防止导入的旧配置用隧道名逃逸目录
func getLogPath(name string) string {
	return filepath.Join(logDir, filepath.Base(name)+".log")
}

// getPIDPath 获取指定隧道 PID 文件的完整路径
func getPIDPath(name string) string {
	return filepath.Join(pidDir, filepath.Base(name)+".pid")
}

// ====== 导入/导出（无 Emoji 版）======
// backupRestoreMenu 配置导入导出管理菜单
// 支持导出备份、导入恢复、查看备份列表功能
func backupRestoreMenu(cfg *Config, homeDir, configPath string, useKeyboard bool, reader *bufio.Reader) {
	if useKeyboard {
		if err := keyboard.Open(); err != nil {
			useKeyboard = false
		} else {
			defer keyboard.Close()
		}
	}

	selected := 1
	for {
		clearScreen()
		width := 80
		if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
			if w, _, err := term.GetSize(fd); err == nil {
				width = w
			}
		}
		drawCenteredTitleBox(ColorCyan+"[ 配置导入/导出 ]"+ColorReset, width)

		if !useKeyboard {
			fmt.Printf("%s[数字模式] 输入数字选择，Q返回%s\n\n", ColorYellow, ColorReset)
		}

		options := []struct {
			index int
			icon  string
			text  string
			desc  string
		}{
			{1, ">", "导出配置（备份）", "备份当前配置到文件"},
			{2, "<", "导入配置（恢复）", "从备份文件恢复配置"},
			{3, "#", "查看备份列表", "列出所有可用备份"},
			{4, "<", "返回上级菜单", "回到主菜单"},
		}

		for _, opt := range options {
			prefix := "   "
			lineColor := ColorReset
			if opt.index == selected {
				prefix = " > "
				lineColor = ColorCyan
			}
			fmt.Printf("%s%s%2d. [%s] %s - %s%s\n",
				prefix, lineColor, opt.index, opt.icon, opt.text, opt.desc, ColorReset)
		}

		var choice int = 0

		if !useKeyboard {
			fmt.Printf("\n%s输入数字(1-4): %s", ColorGreen, ColorReset)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if input == "q" {
				return
			}
			num, err := strconv.Atoi(input)
			if err == nil && num >= 1 && num <= 4 {
				choice = num
			}
		} else {
			fmt.Printf("\n%s[↑↓]选择 [回车]执行 [Q]退出%s\n", ColorGreen, ColorReset)
			char, key, err := keyboard.GetKey()
			if err != nil {
				continue
			}
			switch key {
			case keyboard.KeyArrowUp:
				selected--
				if selected < 1 {
					selected = 4
				}
				continue
			case keyboard.KeyArrowDown:
				selected++
				if selected > 4 {
					selected = 1
				}
				continue
			case keyboard.KeyEnter:
				choice = selected
			case keyboard.KeyEsc:
				return
			default:
				if char == 'q' || char == 'Q' {
					return
				}
				if char >= '1' && char <= '4' {
					choice = int(char - '0')
				}
			}
		}

		if choice > 0 {
			switch choice {
			case 1:
				exportConfig(cfg, homeDir, configPath, reader)
				fmt.Printf("\n%s[按回车继续...]%s", ColorYellow, ColorReset)
				if useKeyboard {
					keyboard.GetKey()
				} else {
					reader.ReadString('\n')
				}
			case 2:
				importConfig(cfg, homeDir, configPath, reader)
				fmt.Printf("\n%s[按回车继续...]%s", ColorYellow, ColorReset)
				if useKeyboard {
					keyboard.GetKey()
				} else {
					reader.ReadString('\n')
				}
			case 3:
				listBackups(homeDir)
				fmt.Printf("\n%s[按回车继续...]%s", ColorYellow, ColorReset)
				if useKeyboard {
					keyboard.GetKey()
				} else {
					reader.ReadString('\n')
				}
			case 4:
				return
			}
		}
	}
}

// exportConfig 将当前配置导出为 tar.gz 压缩包
// 包含隧道配置文件和凭证文件，保存到 ~/.cloudflared-backups/ 目录
func exportConfig(cfg *Config, homeDir, configPath string, reader *bufio.Reader) {
	keyboard.Close()
	defer func() {
		if err := keyboard.Open(); err != nil {
			fmt.Printf("%s[!] 无法恢复键盘监听: %v%s\n", ColorYellow, err, ColorReset)
		}
	}()

	fmt.Print(infoStyle.Render("备份名称 [tunnel-backup]: "))
	name, _ := reader.ReadString('\n')
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tunnel-backup"
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s-%s.tar.gz", name, timestamp)
	backupPath := filepath.Join(homeDir, backupDir, filename)

	os.MkdirAll(filepath.Join(homeDir, backupDir), 0755)

	// [安全] 备份包含证书与隧道密钥，必须 0600 防止同机其他用户读取
	file, err := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Println(errorStyle.Render("[X] 无法创建备份文件: " + err.Error()))
		return
	}
	defer file.Close()

	gw := gzip.NewWriter(file)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	files := []string{
		filepath.Join(homeDir, saveFile),
	}

	entries, _ := os.ReadDir(configPath)
	for _, entry := range entries {
		files = append(files, filepath.Join(configPath, entry.Name()))
	}

	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		header, _ := tar.FileInfoHeader(info, info.Name())
		header.Name = filepath.Base(path)
		tw.WriteHeader(header)

		data, _ := os.ReadFile(path)
		tw.Write(data)
	}

	fmt.Println(successStyle.Render("[OK] 备份完成: " + backupPath))
	fmt.Printf("   包含 %d 个隧道配置\n", len(cfg.Tunnels))
}

// importConfig 从备份文件导入配置
// 支持选择现有备份或直接输入路径，会覆盖当前配置
func importConfig(cfg *Config, homeDir, configPath string, reader *bufio.Reader) {
	keyboard.Close()
	defer func() {
		if err := keyboard.Open(); err != nil {
			fmt.Printf("%s[!] 无法恢复键盘监听: %v%s\n", ColorYellow, err, ColorReset)
		}
	}()

	backupDirPath := filepath.Join(homeDir, backupDir)
	entries, _ := os.ReadDir(backupDirPath)

	if len(entries) == 0 {
		fmt.Println(warningStyle.Render("[!] 没有找到备份文件"))
		fmt.Print(infoStyle.Render("输入备份文件路径: "))
		path, _ := reader.ReadString('\n')
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		extractBackup(path, configPath, homeDir, reader)
	} else {
		fmt.Println(infoStyle.Render("可用备份:"))
		for i, entry := range entries {
			info, _ := entry.Info()
			fmt.Printf("  %d. %s (%s)\n", i+1, entry.Name(), info.ModTime().Format("2006-01-02 15:04"))
		}
		fmt.Print(infoStyle.Render("选择编号或输入路径: "))
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		num, err := strconv.Atoi(input)
		if err == nil && num > 0 && num <= len(entries) {
			extractBackup(filepath.Join(backupDirPath, entries[num-1].Name()), configPath, homeDir, reader)
		} else if input != "" {
			extractBackup(input, configPath, homeDir, reader)
		}
	}
}

// extractBackup 解压并恢复备份文件
// 安全措施：
//  1. 先校验 gzip 头，非法备份直接报错（不再 panic）
//  2. 拒绝绝对路径与 .. 路径穿越，拒绝超大文件（单文件 20MB / 总计 100MB）
//  3. 先完整解压到临时目录，全部成功后才替换现有配置（中途失败不影响现有配置）
func extractBackup(backupPath, configPath, homeDir string, reader *bufio.Reader) {
	fmt.Print(warningStyle.Render("[!] 这将覆盖现有配置，确定吗? [yes/N]: "))
	confirm, _ := reader.ReadString('\n')
	if strings.TrimSpace(strings.ToLower(confirm)) != "yes" {
		fmt.Println(infoStyle.Render("[已取消]"))
		return
	}

	file, err := os.Open(backupPath)
	if err != nil {
		fmt.Println(errorStyle.Render("[X] 无法打开文件: " + err.Error()))
		return
	}
	defer file.Close()

	// [修复] 检查 gzip 错误，避免非 gzip 文件导致 panic
	gr, err := gzip.NewReader(file)
	if err != nil {
		fmt.Println(errorStyle.Render("[X] 不是有效的 gzip 备份文件: " + err.Error()))
		return
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	// 先解压到临时目录，成功后再原子替换
	tmpDir := configPath + ".restore-tmp"
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		fmt.Println(errorStyle.Render("[X] 无法创建临时目录: " + err.Error()))
		return
	}

	const maxFileSize = 20 << 20   // 单个文件 20MB
	const maxTotalSize = 100 << 20 // 总计 100MB
	var totalSize int64
	success := false

	for {
		header, err := tr.Next()
		if err == io.EOF {
			success = true
			break
		}
		if err != nil {
			fmt.Println(errorStyle.Render("[X] 解压中断: " + err.Error()))
			break
		}

		// [安全] 路径穿越防护：拒绝绝对路径与 .. 逃逸
		name := filepath.Clean(header.Name)
		if filepath.IsAbs(header.Name) || name == ".." ||
			strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			fmt.Println(errorStyle.Render("[X] 备份包含非法路径，已中止: " + header.Name))
			break
		}

		// [安全] 大小上限，防止解压炸弹
		if header.Size > maxFileSize || totalSize+header.Size > maxTotalSize {
			fmt.Println(errorStyle.Render("[X] 备份文件过大，已中止"))
			break
		}
		totalSize += header.Size

		target := filepath.Join(tmpDir, name)
		if header.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0755)

		data := make([]byte, header.Size)
		if _, err := io.ReadFull(tr, data); err != nil {
			fmt.Println(errorStyle.Render("[X] 解压中断: " + err.Error()))
			break
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			fmt.Println(errorStyle.Render("[X] 写入失败: " + err.Error()))
			break
		}
	}

	if !success {
		os.RemoveAll(tmpDir)
		fmt.Println(errorStyle.Render("[X] 恢复失败，现有配置未被修改"))
		return
	}

	// 全部解压成功，替换现有配置目录
	os.RemoveAll(configPath)
	if err := os.Rename(tmpDir, configPath); err != nil {
		fmt.Println(errorStyle.Render("[X] 无法替换配置目录，请手动处理: " + tmpDir))
		return
	}

	// 保存文件单独放回主目录
	saveSrc := filepath.Join(configPath, saveFile)
	if _, err := os.Stat(saveSrc); err == nil {
		os.Rename(saveSrc, filepath.Join(homeDir, saveFile))
	}

	fmt.Println(successStyle.Render("[OK] 恢复完成"))
	fmt.Println(infoStyle.Render("[*] 请重新运行程序加载新配置"))
}

// listBackups 列出所有可用的备份文件及其大小和修改时间
func listBackups(homeDir string) {
	backupDirPath := filepath.Join(homeDir, backupDir)
	entries, _ := os.ReadDir(backupDirPath)

	if len(entries) == 0 {
		fmt.Println(warningStyle.Render("[!] 没有备份文件"))
		return
	}

	fmt.Println(infoStyle.Render("备份列表:"))
	for i, entry := range entries {
		info, _ := entry.Info()
		size := float64(info.Size()) / 1024.0
		fmt.Printf("  %2d. [BKUP] %s (%.1f KB, %s)\n",
			i+1, entry.Name(), size, info.ModTime().Format("2006-01-02 15:04"))
	}
}

// certManager HTTPS 证书管理菜单
// 修复：所有用户输入前关闭 keyboard，输入后恢复
func certManager(configPath, binPath string, useKeyboard bool, reader *bufio.Reader) {
	selected := 1

	// 不在函数开头打开 keyboard，而是在需要导航时才打开
	// 这样可以避免与 reader 的冲突

	for {
		clearScreen()
		width := 80
		if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
			if w, _, err := term.GetSize(fd); err == nil {
				width = w
			}
		}
		drawCenteredTitleBox(ColorCyan+"[ HTTPS 证书管理 ]"+ColorReset, width)

		if !useKeyboard {
			fmt.Printf("%s[数字模式] 输入数字选择，Q返回%s\n\n", ColorYellow, ColorReset)
		}

		certPath := filepath.Join(configPath, "cert.pem")
		certExists := false
		if info, err := os.Stat(certPath); err == nil {
			certExists = true
			fmt.Printf(" [文件] %s\n", certPath)
			fmt.Printf(" [时间] %s\n", info.ModTime().Format("2006-01-02 15:04:05"))
			fmt.Printf(" [大小] %.1f KB\n\n", float64(info.Size())/1024.0)
		} else {
			fmt.Print(" [!] 未找到证书文件\n\n")
		}

		options := []struct {
			index int
			icon  string
			text  string
			desc  string
		}{
			{1, "*", "重新登录", "更新 HTTPS 证书"},
			{2, "?", "验证证书", "检查证书有效性"},
			{3, "-", "删除证书", "删除当前证书文件"},
			{4, ">", "备份证书", "备份证书到其他位置"},
			{5, "<", "返回上级", "回到主菜单"},
		}

		for _, opt := range options {
			prefix := "   "
			lineColor := ColorReset
			if opt.index == selected {
				prefix = " > "
				lineColor = ColorCyan
			}
			fmt.Printf("%s%s%2d. [%s] %s - %s%s\n",
				prefix, lineColor, opt.index, opt.icon, opt.text, opt.desc, ColorReset)
		}

		var choice int = 0

		if !useKeyboard {
			fmt.Printf("\n%s输入数字(1-5): %s", ColorGreen, ColorReset)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if input == "q" {
				return
			}
			num, err := strconv.Atoi(input)
			if err == nil && num >= 1 && num <= 5 {
				choice = num
			}
		} else {
			// 在需要读取键盘前打开
			if err := keyboard.Open(); err != nil {
				useKeyboard = false
				continue
			}

			fmt.Printf("\n%s[↑↓]选择 [回车]执行 [Q]退出%s\n", ColorGreen, ColorReset)
			char, key, err := keyboard.GetKey()
			keyboard.Close() // 立即关闭，避免影响后续 reader 操作

			if err != nil {
				continue
			}
			switch key {
			case keyboard.KeyArrowUp:
				selected--
				if selected < 1 {
					selected = 5
				}
				continue
			case keyboard.KeyArrowDown:
				selected++
				if selected > 5 {
					selected = 1
				}
				continue
			case keyboard.KeyEnter:
				choice = selected
			case keyboard.KeyEsc:
				return
			default:
				if char == 'q' || char == 'Q' {
					return
				}
				if char >= '1' && char <= '5' {
					choice = int(char - '0')
				}
			}
		}

		if choice > 0 {
			// 在执行操作前确保 keyboard 已关闭
			keyboard.Close()

			switch choice {
			case 1:
				// 【新增】Android/iOS/Docker 拦截
				if !isBrowserAvailable() {
					fmt.Println(warningStyle.Render("[!] 当前环境不支持浏览器登录"))
					fmt.Println(infoStyle.Render("[*] 请使用 API Token 模式（新建隧道时自动引导）"))
					fmt.Println(infoStyle.Render("    或从其他设备复制 cert.pem 到本机"))
					fmt.Printf("\n%s[按回车继续...]%s", ColorYellow, ColorReset)
					if useKeyboard {
						if err := keyboard.Open(); err == nil {
							keyboard.GetKey()
							keyboard.Close()
						} else {
							reader.ReadString('\n')
						}
					} else {
						reader.ReadString('\n')
					}
					continue
				}

				// 【原有】重新登录代码完全保留
				fmt.Println("[*] 开始重新登录...")
				cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
				cmd := exec.Command(cloudflaredPath, "tunnel", "login")
				if platformInfo.IsDocker {
					cmd = exec.Command("docker", "run", "-it", "--rm",
						"-v", configPath+":/etc/cloudflared",
						"cloudflare/cloudflared:latest", "tunnel", "login")
				}
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Run()
				fmt.Printf("\n%s[按回车继续...]%s", ColorYellow, ColorReset)
				if useKeyboard {
					// 临时打开 keyboard 等待单个按键
					if err := keyboard.Open(); err == nil {
						keyboard.GetKey()
						keyboard.Close()
					} else {
						reader.ReadString('\n')
					}
				} else {
					reader.ReadString('\n')
				}

			case 2:
				cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
				cmd := exec.Command(cloudflaredPath, "tunnel", "list")
				if platformInfo.IsDocker {
					cmd = exec.Command("docker", "run", "--rm",
						"-v", configPath+":/etc/cloudflared",
						"cloudflare/cloudflared:latest", "tunnel", "list")
				}
				output, err := cmd.CombinedOutput()
				if err != nil {
					fmt.Println("[X] 证书无效或已过期")
				} else {
					fmt.Println("[OK] 证书有效")
					fmt.Println(string(output))
				}
				fmt.Printf("\n%s[按回车继续...]%s", ColorYellow, ColorReset)
				if useKeyboard {
					if err := keyboard.Open(); err == nil {
						keyboard.GetKey()
						keyboard.Close()
					} else {
						reader.ReadString('\n')
					}
				} else {
					reader.ReadString('\n')
				}

			case 3:
				// [关键修复] 删除证书时先关闭 keyboard，避免与 reader 冲突导致卡死
				if !certExists {
					fmt.Println("[!] 证书文件不存在")
					time.Sleep(1 * time.Second)
					continue
				}

				fmt.Print("[!] 确定删除证书? [yes/N]: ")
				confirm, _ := reader.ReadString('\n')
				if strings.TrimSpace(strings.ToLower(confirm)) == "yes" {
					if err := os.Remove(certPath); err != nil {
						fmt.Printf("[X] 删除失败: %v\n", err)
					} else {
						fmt.Println("[OK] 已删除")
					}
				} else {
					fmt.Println("[已取消]")
				}
				time.Sleep(1 * time.Second)

			case 4:
				if !certExists {
					fmt.Println("[!] 证书文件不存在，无法备份")
					time.Sleep(1 * time.Second)
					continue
				}
				backupPath := certPath + ".backup." + time.Now().Format("20060102")
				if data, err := os.ReadFile(certPath); err == nil {
					if err := os.WriteFile(backupPath, data, 0600); err != nil {
						fmt.Printf("[X] 备份失败: %v\n", err)
					} else {
						fmt.Println("[OK] 备份完成: " + backupPath)
					}
				} else {
					fmt.Printf("[X] 读取证书失败: %v\n", err)
				}
				time.Sleep(1 * time.Second)

			case 5:
				return
			}
		}
	}
}

// ====== 运行隧道（VPS 优化版）======
// runTunnelDirectly 直接在前台运行隧道（支持交互式切换至后台）
// 修改版：支持纯静默模式，日志只写入文件，终端保持菜单交互
// 修复：确保 keyboard 正确关闭/恢复，避免资源泄漏
// [修改] 支持协议自动检测和智能切换，当首选协议失败时自动尝试其他协议
func runTunnelDirectly(tunnel Tunnel, binPath string, silent bool, useKeyboard bool, reader *bufio.Reader) bool {
	// 启动前确保 keyboard 关闭
	keyboard.Close()
	time.Sleep(100 * time.Millisecond)

	configPath := filepath.Join(homeDir, ".cloudflared")
	if err := validateTunnelConsistency(binPath, configPath, &tunnel); err != nil {
		fmt.Printf("%s[X] 隧道验证失败: %v%s\n", ColorRed, err, ColorReset)
		if !silent {
			waitForReturn(useKeyboard, reader)
		}
		return false
	}

	stopTunnel(tunnel.Name)
	stopDockerTunnel(tunnel.Name)

	fullDomain := tunnel.Subdomain + "." + tunnel.Domain
	if err := forceSyncDNS(binPath, tunnel.Name, fullDomain, reader); err != nil {
		fmt.Printf("%s[!] DNS 同步警告: %v%s\n", ColorYellow, err, ColorReset)
	}

	logPath := getLogPath(tunnel.Name)
	if _, err := os.Stat(logPath); err == nil {
		os.Remove(logPath)
	}

	cloudflaredPath := filepath.Join(binPath, platformInfo.BinaryName)
	configFile := filepath.Join(homeDir, ".cloudflared", tunnel.Name+".yml")

	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		fmt.Printf("%s[X] 配置文件不存在: %s%s\n", ColorRed, configFile, ColorReset)
		waitForReturn(useKeyboard, reader)
		return false
	}

	// [修改] 智能协议切换：尝试多个协议直到成功
	protocols := []string{
		getProtocolForTunnel(tunnel.Name), // 用户偏好或上次成功的
		"quic",                            // 备选1：UDP协议，抗干扰
		"http2",                           // 备选2：TCP协议
		"auto",                            // 最后尝试自动选择
	}

	// 去重
	seen := make(map[string]bool)
	uniqueProtocols := []string{}
	for _, p := range protocols {
		if !seen[p] {
			seen[p] = true
			uniqueProtocols = append(uniqueProtocols, p)
		}
	}

	var started bool
	var lastErr error
	var cmd *exec.Cmd
	var logFile *os.File

	for i, protocol := range uniqueProtocols {
		// 如果不是第一次尝试，显示切换信息
		if i > 0 && !silent {
			fmt.Printf("%s[!] 协议 %s 连接失败，尝试 %s...%s\n",
				ColorYellow, uniqueProtocols[i-1], protocol, ColorReset)
		} else if !silent {
			fmt.Printf("%s[*] 使用协议: %s%s\n", ColorBlue, protocol, ColorReset)
		}

		// 更新配置文件中的协议
		updateConfigProtocol(configFile, protocol)
		saveProtocolPreference(tunnel.Name, protocol)

		// 清理之前的日志
		os.Remove(logPath)

		// 准备命令
		args := []string{"tunnel", "--config", configFile, "run", tunnel.Name}
		cmd = exec.Command(cloudflaredPath, args...)
		procMgr.SetupDaemonProcess(cmd)

		// 打开日志文件
		var err error
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Printf("%s[X] 无法创建日志文件: %v%s\n", ColorRed, err, ColorReset)
			if !silent {
				waitForReturn(useKeyboard, reader)
			}
			return false
		}

		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.Stdin = nil

		// 尝试启动
		if err := cmd.Start(); err != nil {
			logFile.Close()
			lastErr = err
			continue
		}

		// 等待验证启动
		time.Sleep(2 * time.Second)

		// 检查进程是否还在运行
		if !isProcessRunning(cmd.Process.Pid) {
			logFile.Close()
			lastErr = fmt.Errorf("进程启动后立即退出")

			// 检查是否是协议错误
			if !isProtocolBlockedError(logPath) {
				break // 非协议问题，停止重试
			}
			continue
		}

		// 检查是否成功连接或出现协议错误
		protocolFailed := false
		for j := 0; j < 8; j++ { // 最多等待约6秒
			time.Sleep(800 * time.Millisecond)

			// 成功标志
			if checkTunnelConnected(logPath) {
				started = true
				break
			}

			// 协议阻断错误，快速失败
			if isProtocolBlockedError(logPath) {
				protocolFailed = true
				procMgr.KillProcess(cmd.Process.Pid, false)
				time.Sleep(200 * time.Millisecond)
				break
			}

			// 其他致命错误
			if checkTunnelError(logPath) {
				procMgr.KillProcess(cmd.Process.Pid, false)
				lastErr = fmt.Errorf("隧道启动错误")
				protocolFailed = false // 标记为非协议错误，停止重试
				break
			}
		}

		if started {
			if i > 0 && !silent {
				fmt.Printf("%s[OK] 协议 %s 启动成功！%s\n",
					ColorGreen, protocol, ColorReset)
			}
			// 不关闭 logFile，因为进程还在使用
			break
		}

		// 清理失败进程的日志文件
		logFile.Close()
		os.Remove(getPIDPath(tunnel.Name))

		// 如果不是协议错误，停止重试
		if !protocolFailed {
			break
		}
	}

	if !started {
		fmt.Printf("%s[X] 所有协议均无法连接%s\n", ColorRed, ColorReset)
		if lastErr != nil {
			fmt.Printf("   错误: %v\n", lastErr)
		}
		fmt.Printf("   日志: %s\n", logPath)
		if !silent {
			waitForReturn(useKeyboard, reader)
		}
		return false
	}

	// 保存 PID
	os.WriteFile(getPIDPath(tunnel.Name), []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	if !silent {
		fmt.Println(boxStyle.Render(
			"[ 启动隧道: " + tunnel.Name + " ]\n" +
				"[URL] https://" + fullDomain + " -> localhost:" + tunnel.Port,
		))

		if !checkLocalService(tunnel.Port) {
			fmt.Println(warningStyle.Render("[!] 端口 " + tunnel.Port + " 未检测到服务"))
		}

		fmt.Printf("\n%s[*] 隧道正在运行，日志写入: %s%s\n", ColorBlue, logPath, ColorReset)
		fmt.Printf("%s[*] 按 Q 或 ESC 返回主菜单（隧道继续运行）%s\n\n", ColorYellow, ColorReset)
	}

	fmt.Printf("%s[OK] 隧道已启动，PID: %d%s\n", ColorGreen, cmd.Process.Pid, ColorReset)

	if silent {
		// 静默模式下关闭日志文件句柄（但进程继续运行）
		logFile.Close()
		return true
	}

	// 交互模式：等待用户按键
	if useKeyboard {
		// 重新打开 keyboard 用于捕获退出键
		if err := keyboard.Open(); err != nil {
			useKeyboard = false
		} else {
			fmt.Printf("\n%s[按 Q 或 ESC 返回主菜单]...%s", ColorYellow, ColorReset)

			for {
				char, key, err := keyboard.GetKey()
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				if key == keyboard.KeyEsc || char == 'q' || char == 'Q' {
					fmt.Printf("\n%s[*] 返回主菜单，隧道继续后台运行%s\n", ColorBlue, ColorReset)
					break
				}
			}
			keyboard.Close()
		}
	}

	if !useKeyboard {
		fmt.Printf("\n%s[按回车返回主菜单]...%s", ColorYellow, ColorReset)
		reader.ReadString('\n')
	}

	// 关闭日志文件句柄（隧道进程继续运行）
	logFile.Close()

	return true
}

// editTunnel 交互式编辑隧道配置
// 直接回车保持原值，输入新值则更新（新值需通过合法性校验）
func editTunnel(tunnel *Tunnel, reader *bufio.Reader) {
	fmt.Println(infoStyle.Render("编辑隧道 (直接回车保持原值):"))

	for {
		fmt.Printf("域名 [%s]: ", tunnel.Domain)
		if d, _ := reader.ReadString('\n'); strings.TrimSpace(d) != "" {
			d = strings.TrimSpace(d)
			if !isValidDomain(d) {
				fmt.Println(errorStyle.Render("[X] 域名格式无效，未修改"))
				continue
			}
			tunnel.Domain = d
		}
		break
	}

	for {
		fmt.Printf("子域名 [%s]: ", tunnel.Subdomain)
		if s, _ := reader.ReadString('\n'); strings.TrimSpace(s) != "" {
			s = strings.TrimSpace(s)
			if !isValidTunnelName(s) {
				fmt.Println(errorStyle.Render("[X] 子域名仅允许字母/数字/_/- (1-63字符)，未修改"))
				continue
			}
			tunnel.Subdomain = s
		}
		break
	}

	for {
		fmt.Printf("端口 [%s]: ", tunnel.Port)
		if p, _ := reader.ReadString('\n'); strings.TrimSpace(p) != "" {
			p = strings.TrimSpace(p)
			if n, err := strconv.Atoi(p); err != nil || n <= 0 || n > 65535 {
				fmt.Println(errorStyle.Render("[X] 端口无效（1-65535），未修改"))
				continue
			}
			tunnel.Port = p
		}
		break
	}

	for {
		currentScheme := normalizeServiceScheme(tunnel.ServiceScheme)
		fmt.Printf("服务协议 [%s] http/ssh: ", currentScheme)
		if sc, _ := reader.ReadString('\n'); strings.TrimSpace(sc) != "" {
			scheme := strings.ToLower(strings.TrimSpace(sc))
			if scheme != "http" && scheme != "ssh" && scheme != "https" {
				fmt.Println(errorStyle.Render("[X] 协议仅支持 http/https/ssh，未修改"))
				continue
			}
			tunnel.ServiceScheme = scheme
		}
		break
	}

	fmt.Println(successStyle.Render("[OK] 已更新"))
}

// deleteTunnel 删除指定索引的隧道
// 停止相关进程，删除配置文件和凭证文件，从配置中移除
func deleteTunnel(cfg *Config, index int, configPath string) {
	tunnel := cfg.Tunnels[index]

	stopTunnel(tunnel.Name)
	stopDaemon(tunnel.Name)
	stopDockerTunnel(tunnel.Name)

	os.Remove(filepath.Join(configPath, tunnel.Name+".yml"))
	os.Remove(filepath.Join(configPath, tunnel.TunnelID+".json"))

	cfg.Tunnels = append(cfg.Tunnels[:index], cfg.Tunnels[index+1:]...)
	fmt.Println(successStyle.Render("[OK] 已删除: " + tunnel.Name))
}

// ====== 清理配置（无 Emoji 版）======
// cleanConfig 清除所有本地配置
// 停止所有后台进程，删除配置目录和保存文件
func cleanConfig(cfg *Config, homeDir, configPath string, useKeyboard bool, reader *bufio.Reader) {
	if useKeyboard {
		keyboard.Close()
		defer func() {
			if err := keyboard.Open(); err != nil {
				fmt.Printf("%s[!] 无法恢复键盘监听: %v%s\n", ColorYellow, err, ColorReset)
			}
		}()
	}

	fmt.Print(errorStyle.Render("[!] 确定删除所有配置? [yes/N]: "))
	answer, _ := reader.ReadString('\n')

	if strings.TrimSpace(strings.ToLower(answer)) == "yes" {
		for _, t := range cfg.Tunnels {
			stopDaemon(t.Name)
			stopDockerTunnel(t.Name)
		}
		os.RemoveAll(configPath)
		os.Remove(filepath.Join(homeDir, saveFile))
		*cfg = Config{}
		fmt.Println(successStyle.Render("[OK] 已清除所有配置"))
	} else {
		fmt.Println(infoStyle.Render("[已取消]"))
	}

	if !useKeyboard {
		fmt.Printf("\n%s[按回车返回...]%s", ColorYellow, ColorReset)
		reader.ReadString('\n')
	} else {
		time.Sleep(1 * time.Second)
	}
}

// isTunnelRunning 检查隧道是否在前台运行
// 使用 pgrep 和 ps 两种方法检测（兼容 VPS/Android 环境）
func isTunnelRunning(name string) bool {
	return len(procMgr.FindProcessByName(name)) > 0
}

// stopTunnel 停止前台运行的隧道
// 优先使用 PID 文件精确停止，回退到 pkill 模糊匹配
func stopTunnel(name string) {
	pid := getDaemonPID(name)
	if pid > 0 {
		procMgr.KillProcess(pid, false)
		time.Sleep(500 * time.Millisecond)
		if isProcessRunning(pid) {
			procMgr.KillProcess(pid, true)
		}
		os.Remove(getPIDPath(name))
		return
	}
	pids := procMgr.FindProcessByName(name)
	for _, pid := range pids {
		procMgr.KillProcess(pid, true)
	}
}

// checkLocalService 检查本地端口是否有服务在监听
// 依次尝试 TCP 连接、ss 命令、netstat 命令三种检测方式
func checkLocalService(port string) bool {
	conn, err := net.DialTimeout("tcp", "localhost:"+port, 2*time.Second)
	if err == nil {
		conn.Close()
		return true
	}

	cmd := exec.Command("sh", "-c", fmt.Sprintf("ss -ltnp | grep -q :%s", port))
	if cmd.Run() == nil {
		return true
	}

	cmd = exec.Command("sh", "-c", fmt.Sprintf("netstat -tn 2>/dev/null | grep -q ':%s'", port))
	return cmd.Run() == nil
}

// saveConfig 将配置保存到 YAML 文件（权限 0600）
// [修复] 配置文件损坏时拒绝写回（保护原文件）；marshal/写入错误会打印警告
func saveConfig(cfg Config, path string) {
	if configLoadFailed {
		fmt.Printf("%s[!] 配置文件损坏，已跳过保存以保护原文件: %s%s\n",
			ColorYellow, path, ColorReset)
		return
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		fmt.Printf("%s[X] 配置序列化失败: %v%s\n", ColorRed, err, ColorReset)
		return
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		fmt.Printf("%s[X] 配置保存失败: %v%s\n", ColorRed, err, ColorReset)
	}
}

// isLoggedIn 检查是否已登录（ cert.pem 文件存在且非空）
func isLoggedIn(configPath string) bool {
	certFile := filepath.Join(configPath, "cert.pem")
	info, err := os.Stat(certFile)
	return err == nil && info.Size() > 0
}

// addToPath 将二进制目录添加到用户 PATH（写入 .bashrc 或 .zshrc）
func addToPath(binPath string) {
	if strings.Contains(os.Getenv("PATH"), binPath) {
		return
	}
	if runtime.GOOS == "windows" {
		newPath := binPath + ";" + os.Getenv("PATH")
		os.Setenv("PATH", newPath)
		return
	}
	shell := os.Getenv("SHELL")
	rcFile := ".bashrc"
	if strings.Contains(shell, "zsh") {
		rcFile = ".zshrc"
	}
	homeDir, _ := os.UserHomeDir()
	f, _ := os.OpenFile(filepath.Join(homeDir, rcFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	fmt.Fprintf(f, "\nexport PATH=\"%s:$PATH\"\n", binPath)
	os.Setenv("PATH", binPath+":"+os.Getenv("PATH"))
}

// resetTerminal 强制重置终端到标准状态（修复子进程干扰）
// 关键修复：添加平台判断、错误处理、避免竞态条件
func resetTerminal() {
	procMgr.ResetTerminal()
}
