package spv2

// spec §17.3 负向测试：每个"not designed"feature 必须回 not_supported，不得静默成功，
// 也不能退化成 not_found（让调用方以为只是名字拼错）。

import (
	"context"
	"testing"
	"time"
)

func TestNotDesignedFeaturesReturnNotSupported(t *testing.T) {
	base, err := NewCall("w1", AgentRef("a"), ServiceRef("peer"), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	withCapability := base
	withCapability.To = CapabilityRef("cap-1")
	withTenantFrom := base
	withTenantFrom.From.Tenant = "t1"
	withTenantTo := base
	withTenantTo.To.Tenant = "t1"
	withDelegated := base
	withDelegated.From = AgentRef("a").WithAuthority(AuthorityDelegated)
	for _, tc := range []struct {
		name string
		f    Frame
	}{
		{"capability 目标", withCapability},
		{"from.tenant", withTenantFrom},
		{"to.tenant", withTenantTo},
		{"delegated authority", withDelegated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.f.Validate(); !IsCode(err, CodeNotSupported) {
				t.Fatalf("%s 应 not_supported，得到 %v", tc.name, err)
			}
		})
	}

	// 未设计的 method（spawn/drain/retire）在服务端必须回 not_supported。
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	c := wsDialClient(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, method := range []string{"spawn", "drain", "retire"} {
		call, err := NewCall("w-"+method, AgentRef("client"), ServiceRef(wsTestService), method, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Call(ctx, call); !IsCode(err, CodeNotSupported) {
			t.Fatalf("method %s 应 not_supported，得到 %v", method, err)
		}
	}
	if !hasMetric(srv.Metrics().Snapshot(), "sump_errors_total", map[string]string{"code": CodeNotSupported}) {
		t.Fatalf("not_supported 没有进 errors_total: %+v", srv.Metrics().Snapshot())
	}
}

// hasMetric 判断指标快照里是否含指定 label 组合。
func hasMetric(snap []Metric, name string, labels map[string]string) bool {
	for _, m := range snap {
		if m.Name != name {
			continue
		}
		ok := true
		for k, v := range labels {
			if m.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
