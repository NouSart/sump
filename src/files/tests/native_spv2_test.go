package tests

// files 原生 SPv2.1 method 的端到端校验：不经 v1 JumpPayload/Envelope 合成，
// 直接用 /spv2 的 call 帧调 save/read/fetch，并核对服务卡里的方法声明。

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// TestNativeSPV2Methods 验证 files 的 provides 动作都有原生 method，且调用链不走 v1。
func TestNativeSPV2Methods(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)

	target := srv.WsURL()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, target)
	if err != nil {
		t.Fatalf("连接 /spv2 失败: %v", err)
	}
	defer c.Close()

	card, err := c.Hello(ctx, spv2.ServiceRef("test"), spv2.ServiceRef("files"))
	if err != nil {
		t.Fatalf("hello 失败: %v", err)
	}

	// 每个 provides 动作都应有原生 method，且带 Tool/LongRunning/Input/Output 声明。
	wantTool := map[string]bool{
		"save": true, "fetch": false, "read": true, "write": true,
		"edit_word": true, "edit_ppt": true, "edit_pdf": true,
		"outline": true, "read_range": true, "find": true, "patch": true, "verify": true,
	}
	methods := map[string]spv2.Method{}
	for _, m := range card.Methods {
		methods[m.Name] = m
	}
	for name, tool := range wantTool {
		m, ok := methods[name]
		if !ok {
			t.Fatalf("服务卡缺少原生 method %q", name)
		}
		if m.Tool != tool {
			t.Errorf("%s.Tool = %v, want %v", name, m.Tool, tool)
		}
		if m.LongRunning {
			t.Errorf("%s.LongRunning = true, want false（同步回包）", name)
		}
		if len(m.Input) == 0 || len(m.Output) == 0 {
			t.Errorf("%s 的 Input/Output schema 不完整: input=%v output=%v", name, m.Input, m.Output)
		}
	}

	// save：body 是纯 payload，原生 handler 直接 DecodeRaw。
	data := []byte("hello native spv2\n")
	saveCall, err := spv2.NewCall("work-native", spv2.ServiceRef("test"), spv2.ServiceRef("files"), "save",
		protocol.FileSavePayload{Data: base64.StdEncoding.EncodeToString(data), Filename: "native.txt", Mime: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	saveResp, err := c.Call(ctx, saveCall)
	if err != nil {
		t.Fatalf("原生 save 失败: %v", err)
	}
	var sr protocol.FileSaveResult
	if err := saveResp.DecodeBody(&sr); err != nil {
		t.Fatal(err)
	}
	if sr.ID == "" || sr.Filename != "native.txt" || sr.Size != int64(len(data)) {
		t.Fatalf("原生 save 结果不符: %+v", sr)
	}

	// read：原生 method 直接读 body，返回文本。
	readCall, err := spv2.NewCall("work-native", spv2.ServiceRef("test"), spv2.ServiceRef("files"), "read",
		protocol.FileReadPayload{ID: sr.ID})
	if err != nil {
		t.Fatal(err)
	}
	readResp, err := c.Call(ctx, readCall)
	if err != nil {
		t.Fatalf("原生 read 失败: %v", err)
	}
	var rr protocol.FileReadResult
	if err := readResp.DecodeBody(&rr); err != nil {
		t.Fatal(err)
	}
	if rr.Text != string(data) || rr.Truncated {
		t.Fatalf("原生 read 结果不符: %+v", rr)
	}

	// fetch：仍按原语义返回 base64。
	fetchCall, err := spv2.NewCall("work-native", spv2.ServiceRef("test"), spv2.ServiceRef("files"), "fetch",
		protocol.FileFetchPayload{ID: sr.ID})
	if err != nil {
		t.Fatal(err)
	}
	fetchResp, err := c.Call(ctx, fetchCall)
	if err != nil {
		t.Fatalf("原生 fetch 失败: %v", err)
	}
	var fr protocol.FileFetchResult
	if err := fetchResp.DecodeBody(&fr); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil || string(got) != string(data) {
		t.Fatalf("原生 fetch 内容不符: data=%q err=%v", fr.Data, err)
	}

	// 错误路径：不存在的文件应回 not_found，而不是笼统的 internal。
	missingCall, err := spv2.NewCall("work-native", spv2.ServiceRef("test"), spv2.ServiceRef("files"), "fetch",
		protocol.FileFetchPayload{ID: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, missingCall); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("不存在的文件错误码 = %v, want %q", err, spv2.CodeNotFound)
	}
}
