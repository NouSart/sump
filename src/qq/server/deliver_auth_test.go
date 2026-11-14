package server

// spec §6.5 deliver 归属矩阵的同包单测（守卫按文件定位跑 ./server）。
import (
	"testing"

	"github.com/NouSart/sump/spv2"
)

func TestDeliverAuthorizedMatrix(t *testing.T) {
	work := spv2.Work{Agent: spv2.AgentRef("reasoner"), Owner: spv2.HumanRef("owner")}
	if !deliverAuthorized(work, spv2.AgentRef("reasoner").WithAuthority(spv2.AuthorityRuntime)) {
		t.Fatal("agent+runtime 应允许")
	}
	if !deliverAuthorized(work, spv2.HumanRef("owner")) {
		t.Fatal("owner+owner 应允许")
	}
	for _, from := range []spv2.Ref{
		spv2.AgentRef("reasoner"),                                      // 少了 runtime
		spv2.AgentRef("intruder").WithAuthority(spv2.AuthorityRuntime), // 冒充 agent
		spv2.HumanRef("owner").WithAuthority(spv2.AuthorityRuntime),    // human 带 runtime
		spv2.ServiceRef("qq"),                                          // 服务身份不能 deliver
	} {
		if deliverAuthorized(work, from) {
			t.Fatalf("不应允许: %+v", from)
		}
	}
}
