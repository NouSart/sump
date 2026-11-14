package server

// 设置键从 recall.* 改名 context.* 之后，声明里不允许再出现旧前缀（改名不是别名兼容）。

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/store"
)

func TestMemorySettingKeysUseContextPrefix(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{Store: st}, log.New(io.Discard, "", 0))
	contexts := 0
	for _, s := range s.Settings() {
		if strings.HasPrefix(s.Key, "recall.") {
			t.Fatalf("声明里仍有旧键 %q（应为 context.*，不做别名兼容）", s.Key)
		}
		if strings.HasPrefix(s.Key, "context.") {
			contexts += 1
		}
	}
	if contexts != 6 {
		t.Fatalf("context.* 设置项应为 6 个，实际 %d（键：%v）", contexts, s.Settings())
	}
}
