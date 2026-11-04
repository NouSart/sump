package service

import (
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestSettingSnapshotTracksCommittedChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	s := newSecretTestService(t, path)
	s.HandleSecretWithCommit("token", func(v string) error {
		if v == "invalid" {
			return errors.New("invalid")
		}
		return nil
	}, func(string) {})
	s.HandleSecret("other", nil)
	check := func(value string, version uint64) {
		t.Helper()
		got, rev := s.SettingSnapshot("token")
		if got != value || rev != version {
			t.Fatalf("unexpected committed snapshot: version=%d, want=%d", rev, version)
		}
	}
	check("", 0)
	for _, tc := range []struct {
		value   string
		version uint64
	}{{"A", 1}, {"A", 1}, {"B", 2}, {"A", 3}, {"", 4}, {"", 4}, {"A", 5}} {
		if err := s.ApplySetting("token", tc.value); err != nil {
			t.Fatal(err)
		}
		check(tc.value, tc.version)
	}
	if err := s.ApplySetting("other", "B"); err != nil {
		t.Fatal(err)
	}
	check("A", 5)
	if err := s.ApplySetting("token", "invalid"); err == nil {
		t.Fatal("validation unexpectedly succeeded")
	}
	check("A", 5)
	s.writeSecretsHook = func(map[string]string) error { return errors.New("write failure") }
	if err := s.ApplySetting("token", "B"); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	check("A", 5)
	// 重启加载的是新进程版本，不能与旧进程版本比较。
	restarted := newSecretTestService(t, path)
	restarted.HandleSecretWithCommit("token", nil, func(string) {})
	restarted.loadSecrets()
	value, version := restarted.SettingSnapshot("token")
	if value != "A" || version != 1 {
		t.Fatal("loaded snapshot not published")
	}
	restarted.loadSecrets()
	if _, next := restarted.SettingSnapshot("token"); next != version {
		t.Fatal("same loaded value advanced version")
	}
}

func TestSettingSnapshotIsCoherentDuringUpdates(t *testing.T) {
	s := newSecretTestService(t, "-")
	s.HandleSecret("token", nil)
	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					value, version := s.SettingSnapshot("token")
					if version == 0 && value == "" {
						continue
					}
					if value != strconv.FormatUint(version, 10) {
						t.Error("value and version came from different commits")
						return
					}
				}
			}
		})
	}
	for n := 1; n <= 100; n++ {
		if err := s.ApplySetting("token", strconv.Itoa(n)); err != nil {
			t.Error(err)
			break
		}
	}
	close(done)
	readers.Wait()
}
