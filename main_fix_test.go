package main

// 修复验证测试：下载防护 / 解压安全 / 名称校验 / sha256 校验
// 运行: go test -v .
// 测完可删除本文件（不影响构建）。

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 下载防护 ----------

func TestDownloadRejectsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html>captive portal</html>")
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "cloudflared")
	if err := downloadBinaryToFile(srv.Client(), srv.URL, dest); err == nil {
		t.Fatal("HTML 劫持页应被拒绝，实际却成功了")
	} else if !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("错误信息不对: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("被拒绝后不应留下文件")
	}
}

func TestDownloadRejectsSmallFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "tiny") // 4 字节
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "cloudflared")
	if err := downloadBinaryToFile(srv.Client(), srv.URL, dest); err == nil {
		t.Fatal("小于 1MB 的响应该被拒绝")
	}
}

func TestDownloadRejectsTruncated(t *testing.T) {
	// Content-Length 声称 2MB，实际只发 4 字节后断开
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(2<<20))
		w.WriteHeader(200)
		io.WriteString(w, "oops")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // 模拟服务端中断
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "cloudflared")
	err := downloadBinaryToFile(srv.Client(), srv.URL, dest)
	if err == nil {
		t.Fatal("截断的下载应报错")
	}
}

func TestDownloadAcceptsValid(t *testing.T) {
	payload := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, (2<<20)/4) // 2MB 伪 ELF
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "cloudflared")
	if err := downloadBinaryToFile(srv.Client(), srv.URL, dest); err != nil {
		t.Fatalf("合法下载应成功: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, payload) {
		t.Fatal("下载内容不一致")
	}
}

func TestVerifySha256(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "cloudflared")
	data := []byte("fake-binary")
	os.WriteFile(bin, data, 0600)
	sum := fmt.Sprintf("%x  cloudflared\n", sha256.Sum256(data))

	// 哈希匹配 -> nil
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sum)
	}))
	defer okSrv.Close()
	if err := verifySha256(okSrv.Client(), okSrv.URL, bin); err != nil {
		t.Fatalf("匹配时应通过: %v", err)
	}

	// 哈希不匹配 -> 报错
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "0000000000000000000000000000000000000000000000000000000000000000  cloudflared\n")
	}))
	defer badSrv.Close()
	if err := verifySha256(badSrv.Client(), badSrv.URL, bin); err == nil {
		t.Fatal("哈希不匹配应报错")
	}

	// 校验文件不可达 -> 跳过(nil)
	if err := verifySha256(badSrv.Client(), "http://127.0.0.1:1/none", bin); err != nil {
		t.Fatalf("拿不到校验文件应跳过: %v", err)
	}
}

// ---------- 解压安全 ----------

func makeTarGz(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gw.Close()
	return filepath.Join(t.TempDir(), "backup.tar.gz") // 由调用方挪走
}

func writeBackup(t *testing.T, gzData []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup.tar.gz")
	os.WriteFile(p, gzData, 0600)
	return p
}

func TestExtractBackupRejectsNonGzip(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".cloudflared")
	os.MkdirAll(cfg, 0755)
	os.WriteFile(filepath.Join(cfg, "keep.yml"), []byte("x"), 0600)

	bad := writeBackup(t, []byte("not a gzip at all"))
	extractBackup(bad, cfg, dir, bufio.NewReader(strings.NewReader("yes\n")))

	if _, err := os.Stat(filepath.Join(cfg, "keep.yml")); err != nil {
		t.Fatal("非 gzip 导入时原配置不应被清空")
	}
}

func TestExtractBackupRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".cloudflared")
	os.MkdirAll(cfg, 0755)
	os.WriteFile(filepath.Join(cfg, "keep.yml"), []byte("x"), 0600)

	// 构造含路径穿越的 tar.gz
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	evil := "../../evil.txt"
	tw.WriteHeader(&tar.Header{Name: evil, Mode: 0644, Size: 4})
	tw.Write([]byte("pwn"))
	tw.Close()
	gw.Close()
	backup := writeBackup(t, buf.Bytes())

	extractBackup(backup, cfg, dir, bufio.NewReader(strings.NewReader("yes\n")))

	if _, err := os.Stat(filepath.Join(cfg, "keep.yml")); err != nil {
		t.Fatal("路径穿越导入时原配置不应被清空")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("evil.txt 不应写出到配置目录外")
	}
	if _, err := os.Stat(filepath.Join(cfg, "restore-tmp")); !os.IsNotExist(err) {
		t.Fatal("restore-tmp 应被清理")
	}
}

func TestExtractBackupValid(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".cloudflared")
	os.MkdirAll(cfg, 0755)

	// 构造合法备份
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	body := `{"TunnelID":"uuid","TunnelSecret":"s"}`
	tw.WriteHeader(&tar.Header{Name: "abc.json", Mode: 0600, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	gw.Close()
	backup := writeBackup(t, buf.Bytes())

	extractBackup(backup, cfg, dir, bufio.NewReader(strings.NewReader("yes\n")))

	got, err := os.ReadFile(filepath.Join(cfg, "abc.json"))
	if err != nil || string(got) != body {
		t.Fatalf("合法备份应正常恢复: err=%v content=%q", err, got)
	}
}

// ---------- 名称/域名校验 ----------

func TestNameValidation(t *testing.T) {
	valid := []string{"aar-app", "my_tunnel", "T123", strings.Repeat("a", 63)}
	for _, v := range valid {
		if !isValidTunnelName(v) {
			t.Fatalf("%q 应合法", v)
		}
	}
	invalid := []string{"", "bad name", `quo"te`, "../x", "a/b", strings.Repeat("a", 64), "中文"}
	for _, v := range invalid {
		if isValidTunnelName(v) {
			t.Fatalf("%q 应非法", v)
		}
	}
	if !isValidDomain("582550.xyz") || isValidDomain("a b.com") || isValidDomain("nodots") {
		t.Fatal("域名校验行为不对")
	}
}

// ---------- 日志尾部读取 ----------

func TestReadLogTail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.log")
	big := strings.Repeat("x", 100<<10) // 100KB
	os.WriteFile(p, []byte(big+"TAILMARKER"), 0600)
	got := readLogTail(p, 64<<10)
	if !strings.Contains(got, "TAILMARKER") {
		t.Fatal("尾部内容应被读到")
	}
	if len(got) > 70<<10 {
		t.Fatalf("不应整文件读取: %d 字节", len(got))
	}
}
