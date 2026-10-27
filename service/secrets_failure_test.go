// 秘密项落盘失败的三条不变量（见 secrets.go 文件头）：
//   - 落盘失败 = 这次变更没发生：运行时值必须回到旧值；
//   - 失败的值不能留在内存表里，被后续任意一次成功写盘“顺带”持久化；
//   - 从本地文件加载秘密不需要回写文件——文件只读时也要能用。
package service

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func newSecretTestService(t *testing.T, secretFile string) *Service {
	t.Helper()
	return New(Config{
		Name:       "secret-failure-test",
		Listen:     "127.0.0.1:0",
		Center:     "ws://127.0.0.1:1/ws",
		SecretFile: secretFile,
		Logger:     log.New(io.Discard, "", 0),
	})
}

// breakSecretPath 让下一次写盘必败：把目标路径换成目录，与权限和平台无关。
func breakSecretPath(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestSecretWriteFailureRollsBackAppliedValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	s := newSecretTestService(t, path)
	var applied []string
	s.HandleSecret("llm.key", func(v string) error {
		applied = append(applied, v)
		return nil
	})

	if err := s.ApplySetting("llm.key", "old"); err != nil {
		t.Fatal(err)
	}
	breakSecretPath(t, path)

	if err := s.ApplySetting("llm.key", "new"); err == nil {
		t.Fatal("落盘失败时应返回错误")
	}
	if got := s.SettingValue("llm.key"); got != "old" {
		t.Fatalf("落盘失败后生效值应保持 old，实际 %q", got)
	}
	if len(applied) == 0 || applied[len(applied)-1] != "old" {
		t.Fatalf("落盘失败后应把运行时值回滚到 old，实际调用序列 %v", applied)
	}
}

// 启动参数可能在骨架之外直接应用过值（如 reasoner 的 -llm-key）：
// 骨架没有已知旧值，落盘失败时不能回滚成空值、把那个能用的值清掉。
func TestSecretWriteFailureWithoutKnownPreviousDoesNotClearRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	s := newSecretTestService(t, path)
	var applied []string
	s.HandleSecret("llm.key", func(v string) error {
		applied = append(applied, v)
		return nil
	})
	s.writeSecretsHook = func(map[string]string) error {
		return errors.New("read-only")
	}

	if err := s.ApplySetting("llm.key", "from-flag"); err == nil {
		t.Fatal("落盘失败时应返回错误")
	}
	if len(applied) != 1 || applied[0] != "from-flag" {
		t.Fatalf("没有已知旧值时不该回滚成空值，实际调用序列 %v", applied)
	}
}

// 落盘失败时不能把包含候选秘密的临时文件留在磁盘上。
func TestSecretWriteFailureRemovesTempFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	s := newSecretTestService(t, path)
	s.HandleSecret("llm.key", func(string) error { return nil })

	if err := s.ApplySetting("llm.key", "candidate"); err == nil {
		t.Fatal("落盘失败时应返回错误")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("落盘失败后不应残留临时秘密文件（stat err=%v）", err)
	}
}

func TestFailedSecretWriteDoesNotLeakIntoLaterSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	s := newSecretTestService(t, path)
	for _, key := range []string{"llm.key", "other.key", "third.key"} {
		s.HandleSecret(key, func(string) error { return nil })
	}

	if err := s.ApplySetting("llm.key", "A"); err != nil {
		t.Fatal(err)
	}
	breakSecretPath(t, path)
	if err := s.ApplySetting("other.key", "B"); err == nil {
		t.Fatal("落盘失败时应返回错误")
	}

	// 恢复可写（writeSecrets 会重建文件），再成功写另一个键：
	// 失败过的 B 不能被这次成功写盘顺带落盘。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySetting("third.key", "C"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("秘密文件应是合法 JSON: %v / %s", err, raw)
	}
	if got := stored.Secrets["third.key"]; got != "C" {
		t.Fatalf("成功写入的键应在文件里，实际 %q（%s）", got, raw)
	}
	if _, leaked := stored.Secrets["other.key"]; leaked {
		t.Fatalf("落盘失败的值被后续写盘带进了文件: %s", raw)
	}
}

func TestLoadSecretsAppliesWithoutRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	raw, err := json.Marshal(map[string]any{
		"version": 1,
		"secrets": map[string]string{"llm.key": "from-disk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s := newSecretTestService(t, path)
	var applied []string
	s.HandleSecret("llm.key", func(v string) error {
		applied = append(applied, v)
		return nil
	})
	writes := 0
	s.writeSecretsHook = func(map[string]string) error {
		writes++
		return errors.New("read-only")
	}

	s.loadSecrets()

	if got := s.SettingValue("llm.key"); got != "from-disk" {
		t.Fatalf("从文件加载的值应生效（文件只读也要能用），实际 %q", got)
	}
	if writes != 0 {
		t.Fatalf("加载路径不该回写文件，实际写了 %d 次", writes)
	}
	if len(applied) != 1 || applied[0] != "from-disk" {
		t.Fatalf("加载时应用回调应收到文件里的值，实际 %v", applied)
	}
}
