package server

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestNapcatSecretSaveFailureKeepsRuntime(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, candidate := range []string{"fixture-replacement", ""} {
			name := "startup"
			if known {
				name = "saved"
			}
			if candidate == "" {
				name += "_clear"
			} else {
				name += "_replace"
			}
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "secret.json")
				s := New(Config{Name: "qq", NapCatToken: "fixture-original", SecretFile: path}, log.New(io.Discard, "", 0))
				if known {
					if err := s.ApplySetting("napcat.token", "fixture-original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := s.ApplySetting("napcat.token", candidate); err == nil {
					t.Fatal("save unexpectedly succeeded")
				}
				if s.napcatToken != "fixture-original" {
					t.Fatal("failed save changed the active NapCat credential")
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := s.ApplySetting("napcat.token", candidate); err != nil {
					t.Fatal(err)
				}
				if s.napcatToken != candidate || s.SettingValue("napcat.token") != candidate {
					t.Fatal("successful retry not published")
				}
			})
		}
	}
}
