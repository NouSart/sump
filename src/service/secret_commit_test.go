package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecretCommitRunsAfterPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	s := newSecretTestService(t, path)
	runtimeValue := ""
	commits := 0
	s.HandleSecretWithCommit("token", func(string) error { return nil }, func(value string) {
		// 回调可读取已提交值；文件内容也必须先保存成功。
		if s.SettingValue("token") != value {
			t.Fatal("commit ran before effective value changed")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var stored secretFile
		if err := json.Unmarshal(raw, &stored); err != nil || stored.Secrets["token"] != value {
			t.Fatalf("commit ran before persistence: %v", err)
		}
		runtimeValue = value
		commits++
	})
	for _, value := range []string{"A", "B", ""} {
		previous := runtimeValue
		s.writeSecretsHook = func(next map[string]string) error {
			if runtimeValue != previous || s.SettingValue("token") != previous {
				t.Fatal("candidate published before persistence")
			}
			return s.writeSecrets(next)
		}
		if err := s.ApplySetting("token", value); err != nil {
			t.Fatal(err)
		}
		if runtimeValue != value {
			t.Fatal("committed value not published")
		}
	}
	if commits != 3 {
		t.Fatalf("commit called %d times, want 3", commits)
	}
}

func TestSecretCommitFailureDoesNotPublish(t *testing.T) {
	for _, tc := range []struct {
		name, previous, candidate string
	}{{"first_value", "", "B"}, {"replace", "A", "B"}, {"clear", "A", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretTestService(t, "-")
			runtimeValue := ""
			commits := 0
			s.HandleSecretWithCommit("token", nil, func(value string) {
				runtimeValue = value
				commits++
			})
			if tc.previous != "" {
				if err := s.ApplySetting("token", tc.previous); err != nil {
					t.Fatal(err)
				}
			}
			before := commits
			s.writeSecretsHook = func(map[string]string) error { return errors.New("write failed") }
			if err := s.ApplySetting("token", tc.candidate); err == nil {
				t.Fatal("failed write accepted")
			}
			if runtimeValue != tc.previous || s.SettingValue("token") != tc.previous || commits != before {
				t.Fatal("failed write changed committed state")
			}
			s.writeSecretsHook = nil
			if err := s.ApplySetting("token", tc.candidate); err != nil {
				t.Fatal(err)
			}
			if runtimeValue != tc.candidate || commits != before+1 {
				t.Fatal("successful retry did not commit once")
			}
		})
	}
}

func TestSecretCommitValidationRejectsBeforeWrite(t *testing.T) {
	s := newSecretTestService(t, "-")
	s.HandleSecretWithCommit("token", func(string) error { return errors.New("invalid") }, func(string) {
		t.Fatal("rejected value committed")
	})
	s.writeSecretsHook = func(map[string]string) error {
		t.Fatal("rejected value persisted")
		return nil
	}
	if err := s.ApplySetting("token", "invalid"); err == nil {
		t.Fatal("validation error ignored")
	}
	if s.SettingValue("token") != "" {
		t.Fatal("rejected value became effective")
	}
}

func TestSecretCommitLoadsWithoutRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"secrets":{"token":"stored"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := newSecretTestService(t, path)
	var published []string
	s.HandleSecretWithCommit("token", func(value string) error {
		if value != "stored" {
			return errors.New("unexpected loaded value")
		}
		return nil
	}, func(value string) {
		if s.SettingValue("token") != value {
			t.Fatal("loaded value not effective before commit")
		}
		published = append(published, value)
	})
	s.writeSecretsHook = func(map[string]string) error {
		t.Fatal("loading rewrote the secret file")
		return errors.New("read-only")
	}
	s.loadSecrets()
	if len(published) != 1 || published[0] != "stored" {
		t.Fatalf("loaded value not published exactly once: %v", published)
	}
}
