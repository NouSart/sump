package tests

// NapCat 响应形状在 qq 服务层的两种待遇（同一个出口，不能有两套说法）：
//   - 只回 retcode:0、不带 status 的响应是**真成功**——deliver 照样要报成功（修 fail-closed 不该把成功的拒了）；
//   - retcode 非 0 的响应哪怕没有 status 也是失败——deliver 必须带着远端的理由报错，
//     不能"报成功但其实没送出去"（那是这套系统里最坏的失败方向）。
//
// 假端点是本地的 stubNapCat（127.0.0.1 的临时端口），不碰外网。

import (
	"strings"
	"testing"
	"time"
)

// TestDeliverAcceptsRetcodeOnlySuccess 网关只回 retcode 时 deliver 仍然成功。
func TestDeliverAcceptsRetcodeOnlySuccess(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	// 去掉 status，只留 retcode:0（NapCat 部分动作就是这么回的）。
	nc.setReplyShape(func(action string, resp map[string]any) {
		if action == "send_msg" {
			delete(resp, "status")
		}
	})

	rp := deliverOnce(t, qq.WsURL(), "回复内容", "qq:private:2271917353")
	if !rp.OK {
		t.Fatalf("只带 retcode:0 的成功响应被 deliver 判成失败: %s", rp.Error)
	}
	act := nc.waitAction(t, "send_msg", 5*time.Second)
	params, _ := act["params"].(map[string]any)
	if params["message"] != "回复内容" {
		t.Fatalf("deliver 没把消息交给 NapCat: %+v", act)
	}
}

// TestDeliverFailsOnNonzeroRetcodeWithoutStatus retcode 非 0（没有 status）时 deliver 必须失败，
// 并把 retcode 与远端自己写的理由一起带回调用方。
func TestDeliverFailsOnNonzeroRetcodeWithoutStatus(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.setReplyShape(func(action string, resp map[string]any) {
		if action == "send_msg" {
			delete(resp, "status")
			resp["retcode"] = 1400
			resp["message"] = "stub: 网关拒收"
		}
	})

	rp := deliverOnce(t, qq.WsURL(), "回复内容", "qq:private:2271917353")
	if rp.OK {
		t.Fatal("retcode=1400 时 deliver 不应报成功")
	}
	for _, want := range []string{"发送 QQ 消息失败", "retcode=1400", "网关拒收"} {
		if !strings.Contains(rp.Error, want) {
			t.Fatalf("错误文本要含 %q，实际: %q", want, rp.Error)
		}
	}
}

// TestDeliverFailsOnSignallessReply 响应里既没 status 也没 retcode 时无从判定成功，
// 只能按失败上报（不能因为"没说不行"就当行）。
func TestDeliverFailsOnSignallessReply(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.setReplyShape(func(action string, resp map[string]any) {
		if action == "send_msg" {
			delete(resp, "status")
			delete(resp, "retcode")
			resp["data"] = map[string]any{"message_id": 123}
		}
	})

	rp := deliverOnce(t, qq.WsURL(), "回复内容", "qq:private:2271917353")
	if rp.OK {
		t.Fatal("两个信号都没有的响应不该被判成成功")
	}
	if !strings.Contains(rp.Error, "retcode") {
		t.Fatalf("错误该说清缺了哪些信号，实际: %q", rp.Error)
	}
}
