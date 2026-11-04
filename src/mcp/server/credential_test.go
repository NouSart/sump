package server

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const rotatedGatewayToken = "rotated-gateway-token-01234567890123456789"

// 只模拟计时器的受理与取消，便于观察是否真的派发了取消请求。
func credentialJobGateway(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	var cancellations atomic.Int32
	peer := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		var jump protocol.JumpPayload
		if err := env.DecodePayload(&jump); err != nil {
			return nil, err
		}
		switch jump.Action {
		case "sleep":
			return protocol.TimerSleepResult{ItemID: protocol.NewID(), Phase: "queued"}, nil
		case "cancel":
			var input protocol.TimerCancelPayload
			if err := protocol.DecodeRaw(jump.Input, &input); err != nil {
				return nil, err
			}
			cancellations.Add(1)
			return protocol.TimerCancelResult{ItemID: input.ItemID, Status: "canceled"}, nil
		default:
			return nil, errors.New("unexpected timer action")
		}
	})
	s := startGateway(t, Config{}, []protocol.ServiceCard{timerCard(peer)}, `{"exports":["timer__sleep","timer__cancel"]}`)
	return s, &cancellations
}

func TestFailedCredentialRotationPreservesJobAccess(t *testing.T) {
	for _, tc := range []struct {
		name, candidate string
	}{{"replace", rotatedGatewayToken}, {"clear", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			s, cancellations := credentialJobGateway(t)
			client := gatewayClient(t, s, gatewayToken)
			id, uri := jobCallIDs(t, gatewayCall(t, client, "timer__sleep", map[string]any{"ms": 1000}))
			readJob(t, client, uri)
			// 仅破坏测试临时文件的目标类型，稳定触发写盘失败，不依赖 OS 权限。
			if err := os.Remove(s.cfg.SecretFile); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(s.cfg.SecretFile, 0700); err != nil {
				t.Fatal(err)
			}
			if err := s.svc.ApplySetting("http.token", tc.candidate); err == nil {
				t.Fatal("secret persistence unexpectedly succeeded")
			}
			if _, err := s.verifyToken(t.Context(), gatewayToken, nil); err != nil {
				t.Fatalf("previous token rejected after failed rotation: %v", err)
			}
			if _, err := s.verifyToken(t.Context(), tc.candidate, nil); err == nil {
				t.Fatal("failed candidate authorized")
			}
			// 同一令牌的新连接必须仍能读取和取消轮换前的工作。
			reconnected := gatewayClient(t, s, gatewayToken)
			if out := readJob(t, reconnected, uri); out.Final {
				t.Fatal("credential rollback completed native work")
			}
			if result := gatewayCall(t, reconnected, "timer__cancel", map[string]any{"job_id": id}); result.IsError {
				t.Fatalf("failed rotation lost cancellation access: %+v", result)
			}
			if out := readJob(t, client, uri); !out.Final || out.Phase != "canceled" {
				t.Fatalf("cancellation result lost: %+v", out)
			}
			if cancellations.Load() != 1 {
				t.Fatalf("native cancellation count: %d", cancellations.Load())
			}
		})
	}
}

func TestSuccessfulCredentialRoundTripDoesNotRestoreJobAccess(t *testing.T) {
	for _, tc := range []struct {
		name, intermediate string
	}{{"replace", rotatedGatewayToken}, {"clear", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			s, cancellations := credentialJobGateway(t)
			oldClient := gatewayClient(t, s, gatewayToken)
			id, uri := jobCallIDs(t, gatewayCall(t, oldClient, "timer__sleep", map[string]any{"ms": 1000}))
			if err := s.svc.ApplySetting("http.token", tc.intermediate); err != nil {
				t.Fatal(err)
			}
			if _, err := oldClient.ListTools(t.Context(), nil); err == nil {
				t.Fatal("previous token remained authorized after successful rotation")
			}
			if err := s.svc.ApplySetting("http.token", gatewayToken); err != nil {
				t.Fatal(err)
			}
			current := gatewayClient(t, s, gatewayToken)
			if _, err := current.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri}); err == nil {
				t.Fatal("successful round trip restored old job access")
			}
			if result := gatewayCall(t, current, "timer__cancel", map[string]any{"job_id": id}); !result.IsError {
				t.Fatal("successful round trip restored old cancellation access")
			}
			if cancellations.Load() != 0 {
				t.Fatal("revoked job cancellation reached native timer")
			}
			newID, newURI := jobCallIDs(t, gatewayCall(t, current, "timer__sleep", map[string]any{"ms": 1000}))
			readJob(t, current, newURI)
			if result := gatewayCall(t, current, "timer__cancel", map[string]any{"job_id": newID}); result.IsError {
				t.Fatal("current credential could not cancel its new job")
			}
			if cancellations.Load() != 1 {
				t.Fatalf("native cancellation count: %d", cancellations.Load())
			}
		})
	}
}

func TestUnchangedCredentialPreservesJobAccess(t *testing.T) {
	s, cancellations := credentialJobGateway(t)
	client := gatewayClient(t, s, gatewayToken)
	id, uri := jobCallIDs(t, gatewayCall(t, client, "timer__sleep", map[string]any{"ms": 1000}))
	if err := s.svc.ApplySetting("http.token", gatewayToken); err != nil {
		t.Fatal(err)
	}
	reconnected := gatewayClient(t, s, gatewayToken)
	readJob(t, reconnected, uri)
	if result := gatewayCall(t, reconnected, "timer__cancel", map[string]any{"job_id": id}); result.IsError {
		t.Fatal("saving unchanged credential lost cancellation access")
	}
	if cancellations.Load() != 1 {
		t.Fatalf("native cancellation count: %d", cancellations.Load())
	}
}
