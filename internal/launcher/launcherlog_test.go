package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLauncherLogWritesAndKeepsRetention(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// 先放 12 个更早的日志文件：启动新日志后只能留下最新的 10 个。
	start := time.Now().Add(-time.Hour)
	for i := 0; i < 12; i++ {
		name := filepath.Join(dir, fmt.Sprintf("launcher-%d.log", 1700000000000000000+int64(i)))
		if err := os.WriteFile(name, []byte("old\n"), 0600); err != nil {
			t.Fatal(err)
		}
		at := start.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(name, at, at); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{root: root}
	m.startLauncherLog()
	if m.logState == nil {
		t.Fatal("启动器日志文件没有打开")
	}
	if dir := m.launcherLogDir(); dir != filepath.Join(root, "logs") {
		t.Fatalf("launcherLogDir = %q", dir)
	}

	m.appendLauncherLog("第一行")
	// 控制字符不能把一行拆成多行，否则日志文件会被输出带偏。
	m.appendLauncherLog("第二行\r\n第三行")
	m.closeLauncherLog()
	m.closeLauncherLog() // 允许重复关闭
	if m.logState != nil {
		t.Fatal("关闭后应把日志状态置空")
	}
	m.appendLauncherLog("关闭后的行") // 关闭后再写不能 panic

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, launcherLogPrefix) && strings.HasSuffix(name, launcherLogSuffix) {
			kept++
		}
	}
	if kept != launcherLogKeep {
		t.Fatalf("保留了 %d 个日志文件，期望 %d", kept, launcherLogKeep)
	}
	// 本次运行的日志是目录里最新的那个：另外 12 个的时间戳都被改到一小时前。
	newest := ""
	var newestAt time.Time
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, launcherLogPrefix) || !strings.HasSuffix(name, launcherLogSuffix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		if info.ModTime().After(newestAt) {
			newest, newestAt = name, info.ModTime()
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, newest))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "第一行\n第二行第三行\n" {
		t.Fatalf("写入内容 = %q", got)
	}
}

// TestOpenWiresSinkToLauncherLog 验证 Open 真的把 sessionLog 的 sink 接到文件上：
// 这是一整条链路（startLauncherLog → sink → appendLauncherLog），单元函数分别测过，
// 但只有从这里能确认它们被接起来了。
func TestOpenWiresSinkToLauncherLog(t *testing.T) {
	if !supported() {
		t.Skip("实例锁仅在支持的平台实现")
	}
	root := t.TempDir()
	m, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.logMessage("测试写入")
	m.Close() // 等待写入队列清空后再读文件。

	dir := filepath.Join(root, "logs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	written := ""
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), launcherLogPrefix) || !strings.HasSuffix(entry.Name(), launcherLogSuffix) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "[NeoBot Desktop] 测试写入") {
			written = entry.Name()
		}
	}
	if written == "" {
		t.Fatalf("启动器日志里没有写入内容：%v", entries)
	}
}
