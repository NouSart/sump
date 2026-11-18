package tests

// 角色名片：每个进程只暴露本域 method；内部 method 不进门面。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func cardOf(t *testing.T, url, name string) spv2.Card {
	t.Helper()
	conn, err := spv2.Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frame, err := spv2.NewCall("trace-card", spv2.ServiceRef("test"), spv2.ServiceRef(name), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := conn.Call(ctx, frame)
	if err != nil {
		t.Fatal(err)
	}
	var hello spv2.HelloResponse
	if err := protocol.DecodeRaw(resp.Body, &hello); err != nil {
		t.Fatal(err)
	}
	return hello.Card
}

func methodSet(card spv2.Card) map[string]bool {
	out := map[string]bool{}
	for _, m := range card.Methods {
		out[m.Name] = true
	}
	return out
}

func TestRoleCardsAreDisjoint(t *testing.T) {
	sc := startStubCenter(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sh, err := shared.Open(filepath.Join(dir, "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })
	sess := startRole(t, server.RoleSession, "", st, nil, nil)
	exp := startRole(t, server.RoleExperience, "", nil, sh, nil)
	facade := startRole(t, server.RoleFacade, sc.URL(), nil, nil, map[string]string{
		"session": sess.WsURL(), "experience": exp.WsURL(),
	})

	sm := methodSet(cardOf(t, sess.WsURL(), "session"))
	if !sm["store"] || sm["remember"] || sm["context"] {
		t.Fatalf("session 名片不符: %v", sm)
	}
	em := methodSet(cardOf(t, exp.WsURL(), "experience"))
	if !em["remember"] || em["store"] || em["context"] {
		t.Fatalf("experience 名片不符: %v", em)
	}
	fm := methodSet(cardOf(t, facade.WsURL(), "memory"))
	if !fm["context"] || !fm["store"] || !fm["remember"] {
		t.Fatalf("门面应聚合读写方法: %v", fm)
	}
	for _, internal := range []string{"context_base", "inject", "settings_schema", "staged_pending", "staged_mark", "staged_retry"} {
		if fm[internal] {
			t.Fatalf("内部 method %s 不得出现在门面名片", internal)
		}
	}
}
