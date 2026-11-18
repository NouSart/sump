package tests

// memory 域的三进程启动 helper：session（私有库）+ experience（共享库）+ facade。
// e2e 里所有“起 memory”的地方都走这里，避免再回到已退役的单进程形态。

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// startMemoryStack 起 memory 三进程并返回门面 URL。
// peers 里可带 embedding / reasoner 的静态地址（后启动的服务也能先用已知端口）。
func startMemoryStack(t *testing.T, centerURL string, peers map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	return startMemoryStackAt(t, centerURL, peers,
		filepath.Join(dir, "memory.db"), filepath.Join(dir, "experience.db"))
}

// startMemoryStackAt 与 startMemoryStack 相同，但由调用方指定两份库路径（用于检查库隔离）。
func startMemoryStackAt(t *testing.T, centerURL string, peers map[string]string, dbFile, sharedFile string) string {
	t.Helper()
	bin := buildService(t, "github.com/NouSart/sump/memory")

	sessPort := freePort(t)
	expPort := freePort(t)
	memPort := freePort(t)
	sessURL := spv2URL(sessPort)
	expURL := spv2URL(expPort)
	memURL := spv2URL(memPort)

	startProc(t, bin, "-role", "session",
		"-addr", fmt.Sprintf("127.0.0.1:%d", sessPort),
		"-db", dbFile, "-shared-db", sharedFile)
	waitWSReady(t, sessURL, 60*time.Second)

	expPeers := "session=" + sessURL
	for name, addr := range peers {
		expPeers += "," + name + "=" + addr
	}
	startProc(t, bin, "-role", "experience",
		"-addr", fmt.Sprintf("127.0.0.1:%d", expPort),
		"-db", dbFile, "-shared-db", sharedFile, "-peers", expPeers)
	waitWSReady(t, expURL, 60*time.Second)

	startProc(t, bin, "-role", "facade",
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", dbFile, "-shared-db", sharedFile,
		"-peers", "session="+sessURL+",experience="+expURL)
	waitWSReady(t, memURL, 60*time.Second)
	return memURL
}
