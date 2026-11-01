package tests

// 精确指纹的端到端回归：outline 给出的 exact_digest / exact_anchor 必须能一路带到 patch，
// 旧精确锚点再用一次必须整批拒绝，文件不变。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// TestPatchExactAnchorEndToEnd 服务层要能把 outline 给的精确指纹一路带到 patch。
func TestPatchExactAnchorEndToEnd(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveDocx(t, srv.WsURL(), "精确锚点.docx", makeDocx(t, "金额 100 元"))
	ol := outline(t, srv.WsURL(), id, 1)
	if ol.ExactDigest == "" || ol.Nodes[0].ExactAnchor == "" {
		t.Fatalf("outline 没返回精确指纹: %+v", ol)
	}

	rp := call(t, srv.WsURL(), "patch", protocol.DocPatchPayload{
		ID: id, ExactAnchor: ol.ExactDigest,
		Operations: []protocol.DocPatchOp{{
			Op: "replace_text", Node: "body/p1",
			Range: &protocol.DocRange{Start: 3, End: 6}, Text: "200",
			ExactAnchor: ol.Nodes[0].ExactAnchor,
		}},
	})
	if !rp.OK {
		t.Fatalf("带精确锚点的 patch 失败: %s", rp.Error)
	}
	var pr protocol.DocPatchResult
	if err := protocol.DecodeRaw(rp.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Digest == "" || pr.ExactDigest == "" {
		t.Fatalf("patch 结果缺少指纹: %+v", pr)
	}
	if got := readFile(t, srv.WsURL(), id).Text; got != "金额 200 元" {
		t.Fatalf("改后文本 = %q", got)
	}

	// 同一份旧精确锚点再用一次：必须整批拒绝，文件不变。
	rp = call(t, srv.WsURL(), "patch", protocol.DocPatchPayload{
		ID: id, ExactAnchor: ol.ExactDigest,
		Operations: []protocol.DocPatchOp{{
			Op: "replace_text", Node: "body/p1", Text: "改坏",
			ExactAnchor: ol.Nodes[0].ExactAnchor,
		}},
	})
	if rp.OK || !strings.Contains(rp.Error, "exact_digest") && !strings.Contains(rp.Error, "已变化") {
		t.Fatalf("旧精确锚点应被拒绝: %+v", rp)
	}
	if got := readFile(t, srv.WsURL(), id).Text; got != "金额 200 元" {
		t.Fatalf("被拒绝的 patch 改动了文件: %q", got)
	}
}
