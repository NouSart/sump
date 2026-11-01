package tests

// 服务层并发根因实验：
//   - 不同单元格同时写入：锁必须覆盖完整的读出-修改-写回，否则后写覆盖先写。
//   - 同一整篇 digest 同时 patch：乐观锁必须保证只有一个请求基于旧地图改成功。
//
// 请求辅助不使用 t.Fatal：testing.T 的 FailNow 只能从测试 goroutine 调用；
// 并发请求只把错误带回主 goroutine 统一断言。

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/NouSart/sump/protocol"
)

// callQuiet 与 call 同语义，但返回错误供 goroutine 使用。
func callQuiet(addr, action string, input any) (protocol.ResponsePayload, error) {
	conn, err := protocol.Dial(addr, nil)
	if err != nil {
		return protocol.ResponsePayload{}, err
	}
	defer conn.Close()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		return protocol.ResponsePayload{}, err
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "test", "files", "trace-concurrent", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		return protocol.ResponsePayload{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := conn.Call(ctx, env)
	if err != nil {
		return protocol.ResponsePayload{}, err
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		return protocol.ResponsePayload{}, err
	}
	return rp, nil
}

// fetchBytes 取回权威副本的二进制。
func fetchBytes(t *testing.T, addr, id string) []byte {
	t.Helper()
	rp := call(t, addr, "fetch", protocol.FileFetchPayload{ID: id})
	if !rp.OK {
		t.Fatalf("fetch 失败: %s", rp.Error)
	}
	var fr protocol.FileFetchResult
	if err := protocol.DecodeRaw(rp.Data, &fr); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestConcurrentXlsxWritesKeepAllCells 并发写不同单元格后逐格核对：
// 只要锁没覆盖读-改-写全流程，就必然有请求基于旧文件写回并吞掉别人的修改。
func TestConcurrentXlsxWritesKeepAllCells(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveXlsx(t, srv.WsURL(), "并发.xlsx", makeXlsx(t))

	const writers = 12
	cells := make([]string, writers)
	wants := make([]string, writers)
	for i := range cells {
		col, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			t.Fatal(err)
		}
		cells[i] = fmt.Sprintf("%s5", col)
		wants[i] = fmt.Sprintf("并发值-%02d", i)
	}

	start := make(chan struct{})
	results := make([]struct {
		rp  protocol.ResponsePayload
		err error
	}, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rp, err := callQuiet(srv.WsURL(), "write", protocol.FileWritePayload{
				ID: id, Sheet: "Sheet1",
				Edits: []protocol.FileWriteEdit{{Cell: cells[i], Value: wants[i]}},
			})
			results[i].rp, results[i].err = rp, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, res := range results {
		if res.err != nil {
			t.Fatalf("写 %s 请求失败: %v", cells[i], res.err)
		}
		if !res.rp.OK {
			t.Fatalf("写 %s 被拒: %s", cells[i], res.rp.Error)
		}
		var wr protocol.FileWriteResult
		if err := protocol.DecodeRaw(res.rp.Data, &wr); err != nil {
			t.Fatalf("写 %s 结果解码失败: %v", cells[i], err)
		}
		if wr.Updated != 1 {
			t.Fatalf("写 %s 的 Updated=%d，期望 1", cells[i], wr.Updated)
		}
	}

	f, err := excelize.OpenReader(bytes.NewReader(fetchBytes(t, srv.WsURL(), id)))
	if err != nil {
		t.Fatalf("打开编辑后的 xlsx 失败: %v", err)
	}
	defer func() { _ = f.Close() }()
	for i, cell := range cells {
		got, err := f.GetCellValue("Sheet1", cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != wants[i] {
			t.Fatalf("%s 丢了并发写入：实际 %q，期望 %q（某个请求基于旧文件覆盖了它）", cell, got, wants[i])
		}
	}
	// 未被编辑的原有格不能被并发写顺手抹掉。
	if got, _ := f.GetCellValue("Sheet1", "C3"); got != "上海" {
		t.Fatalf("未编辑的 C3 被改变: %q", got)
	}
}

// TestConcurrentPatchSameDigestAllowsExactlyOne 两个请求拿同一份旧 digest 同时 patch：
// 成功者建立新 digest，失败者必须收到"文件已变化"，最终内容只能来自成功者。
func TestConcurrentPatchSameDigestAllowsExactlyOne(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveDocx(t, srv.WsURL(), "并发.docx", makeDocx(t, "原始内容"))
	digest := outline(t, srv.WsURL(), id, 1).Digest

	texts := []string{"请求甲", "请求乙"}
	start := make(chan struct{})
	results := make([]struct {
		rp  protocol.ResponsePayload
		err error
	}, len(texts))
	var wg sync.WaitGroup
	for i := range texts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rp, err := callQuiet(srv.WsURL(), "patch", protocol.DocPatchPayload{
				ID: id, Anchor: digest,
				Operations: []protocol.DocPatchOp{{
					Op: "replace_text", Node: "body/p1", Text: texts[i],
				}},
			})
			results[i].rp, results[i].err = rp, err
		}(i)
	}
	close(start)
	wg.Wait()

	okIdx := -1
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("请求 %s 失败: %v", texts[i], res.err)
		}
		if res.rp.OK {
			if okIdx >= 0 {
				t.Fatalf("两个请求拿着同一旧 digest 都成功了（%s 与 %s），乐观锁未生效", texts[okIdx], texts[i])
			}
			okIdx = i
			continue
		}
		if !strings.Contains(res.rp.Error, "已变化") {
			t.Fatalf("失败请求应说明文件已变化，实际: %s", res.rp.Error)
		}
	}
	if okIdx < 0 {
		t.Fatal("两个请求都失败；旧 digest 应允许其中一个成功")
	}

	rr := readRangeNode(t, srv.WsURL(), id, "body/p1")
	if rr.Text != texts[okIdx] {
		t.Fatalf("最终内容 %q 不是成功请求 %q 的结果", rr.Text, texts[okIdx])
	}
}

// readRangeNode 读取单节点文本。
func readRangeNode(t *testing.T, addr, id, node string) protocol.DocReadResult {
	t.Helper()
	rp := call(t, addr, "read_range", protocol.DocReadPayload{ID: id, Node: node})
	if !rp.OK {
		t.Fatalf("read_range 失败: %s", rp.Error)
	}
	var rr protocol.DocReadResult
	if err := protocol.DecodeRaw(rp.Data, &rr); err != nil {
		t.Fatal(err)
	}
	return rr
}
