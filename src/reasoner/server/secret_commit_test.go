package server

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestLLMSecretSaveFailureKeepsRuntime(t *testing.T) {
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
				s := New(Config{Name: "reasoner", LLMKey: "fixture-original", SecretFile: path}, log.New(io.Discard, "", 0))
				if known {
					if err := s.ApplySetting("llm.key", "fixture-original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				before := s.llmClient()
				if err := s.ApplySetting("llm.key", candidate); err == nil {
					t.Fatal("save unexpectedly succeeded")
				}
				if s.llmKey != "fixture-original" || s.llmClient() != before {
					t.Fatal("failed save changed the active LLM credential or client")
				}
				// 路径恢复后，同一候选值应能正常提交。
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := s.ApplySetting("llm.key", candidate); err != nil {
					t.Fatal(err)
				}
				if s.llmKey != candidate || s.SettingValue("llm.key") != candidate {
					t.Fatal("successful retry not published")
				}
				if (s.llmClient() == nil) != (candidate == "") {
					t.Fatal("client enabled state does not match committed key")
				}
			})
		}
	}
}
