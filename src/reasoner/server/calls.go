package server

// R-030：把工具派发接到 memory 的调用账本（S-001 §3.0.2 / §4.3）。
//
// 只对「受理即回执」（Provide.LongRunning）的动作开 call：这类动作有 owner 句柄
// （item_id）和后续补报，才是 owner 控制 / 对账的对象；同步工具没有句柄也没有补报，
// 开一条永远停在 created 的 call 只会让对账把它误判成「派发未确认」。
//
// 与 S-001 §3.0.2 字面的偏差（见 reports/R-030-dispatch-wiring.md）：owner 载荷不加
// call_id，也不要求 owner 在补报里回传。当前 owner 报告经 agent 中转，agent 用
// (source, trace, item_id) 找回本地 call_id，再把 owner 的 item_id 作为
// remote_item_id 随补报交还 memory。四个 owner 服务一个字段都不用改。
//
// 失败一律可见、且不造假：call_open 失败只记日志 + 指标，派发照常但没有 call_id；
// 没有 call_id 时**绝不**补报（不拿假 id 去写账本）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

const (
	callOpenFailureMetric   = "sump_call_open_failures_total"
	callReportFailureMetric = "sump_call_report_failures_total"
)

// errReportNotMappable 表示这条 owner 补报的 phase/final 不能驱动 memory 账本：
// 内容照旧交付给模型，但不往账本里写半真半假的状态。
var errReportNotMappable = errors.New("补报 phase 不能驱动调用账本")

// asyncReportKey 是一条补报在 memory 侧的幂等键（report_id + 该 call 内的 seq）。
// 同一份 owner 报告重试时复用同一对值，memory 的 report_receipts 才能去重。
type asyncReportKey struct {
	id  string
	seq int64
}

// openToolCall 在派发前向 memory 申请 call_id。失败只记日志与指标并返回空串：
// 派发继续（工具本身不依赖账本），但空串意味着本次调用后续的补报全部跳过。
func (s *Server) openToolCall(ctx context.Context, svc, action, trace string, p protocol.Provide) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := s.Call(cctx, s.memoryName(), "call_open", protocol.CallOpenPayload{
		Work:    trace,
		Service: svc,
		Action:  action,
		// 没有服务声明「常驻」：LongRunning 只表示受理即回执，不是 resident 归属，
		// 因此一律 once；pausable 由 long_running 保守推定（pause 是否真支持由 owner 回执决定）。
		Mode:           "once",
		Pausable:       p.LongRunning,
		ExecDeadlineMS: s.toolTimeout().Milliseconds(),
	}, trace)
	if err != nil {
		s.recordCallFailure(callOpenFailureMetric, svc, "")
		s.logger.Printf("call_open 失败（派发继续，控制退化为不可用）: %s/%s work=%s: %v", svc, action, trace, err)
		return ""
	}
	var res protocol.CallOpenResult
	if err := protocol.DecodeRaw(data, &res); err != nil || strings.TrimSpace(res.CallID) == "" {
		s.recordCallFailure(callOpenFailureMetric, svc, "")
		s.logger.Printf("call_open 回执无效（派发继续）: %s/%s work=%s: err=%v call_id=%q",
			svc, action, trace, err, res.CallID)
		return ""
	}
	return res.CallID
}

// relayAcceptance 把 owner 的受理回执作为一条 accepted 补报交给 memory：
// 同时回填 calls.remote_item_id，并给「已受理、结果未到」一个账本状态（exec=running）。
// 失败不阻塞派发：owner 已经受理，下一次补报仍会带同一个句柄，账本可以自愈。
func (s *Server) relayAcceptance(j *asyncJob, item, receiptText string) {
	if j == nil || j.memCallID == "" || strings.TrimSpace(item) == "" {
		return
	}
	// 受理回执不是「工具输出」：不推 progress，避免界面多一条 JSON 噪声。
	if _, err := s.relayReport(j, "accepted", item, "accepted", false, receiptText, nil, false); err != nil {
		s.logger.Printf("会话 %s：受理补报 relay 失败（派发继续，后续补报可自愈）: %v", j.cid, err)
	}
}

// relayReport 把一条 owner 补报写成 memory 的 store(tool_result, report{...})。
// 返回 stored=true 表示消息与 report receipt 已入账（终态时调用方不必再走普通 record）。
// notifyProgress 与旧 record 路径同口径：入口方声明了 progress 才推实况。
func (s *Server) relayReport(j *asyncJob, key, item, ownerPhase string, final bool, text string, data []byte, notifyProgress bool) (bool, error) {
	if j == nil || j.memCallID == "" {
		return false, nil // 没有账本对象：不补报，也不用假 call_id
	}
	phase, ok := reportPhaseOf(ownerPhase, final)
	if !ok {
		return false, fmt.Errorf("%w: phase=%s final=%v", errReportNotMappable, ownerPhase, final)
	}
	reportID, seq := j.nextReport(key)
	content, truncated := clampRecord(text)
	metaFields := map[string]any{
		"tool": j.name, "call_id": j.callID, "item_id": item, "phase": ownerPhase,
	}
	if truncated {
		metaFields["truncated"] = true
		metaFields["full_chars"] = len([]rune(text))
	}
	meta, err := json.Marshal(metaFields)
	if err != nil {
		return false, fmt.Errorf("编码补报 meta: %w", err)
	}
	payload := protocol.StorePayload{
		ConversationID: j.cid, Work: j.trace, TaskID: j.taskID, Boss: j.boss,
		Role: "tool", Kind: kindToolResult, Content: content, Meta: string(meta),
		// S-001 §3.0 规则 5：补报的幂等键 = report:<source_service>:<report_id>。
		ClientItemID: "report:" + j.source + ":" + reportID,
		Report: &protocol.StoreReport{
			SourceService: j.source, ReportID: reportID, ReportSeq: seq,
			CallID: j.memCallID, Phase: phase, Final: final, Data: data,
			RemoteItemID: item,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := s.Call(ctx, s.memoryName(), "store", payload, j.trace)
	if err != nil {
		s.recordCallFailure(callReportFailureMetric, j.source, phase)
		return false, fmt.Errorf("store 补报（%s item=%s phase=%s）: %w", j.source, item, phase, err)
	}
	var res protocol.StoreResult
	if err := protocol.DecodeRaw(raw, &res); err != nil {
		// 消息已入账，只是拿不到会话内 seq：progress 是展示面，丢一条比把补报判失败好。
		s.logger.Printf("会话 %s：补报回执无法解码（不推 progress）: %v", j.cid, err)
		return true, nil
	}
	if notifyProgress && s.bossWantsProgress(j.boss) {
		s.Fire(j.boss, "progress", protocol.ProgressPayload{
			ConversationID: j.cid, TaskID: j.taskID, Seq: res.Seq, Kind: kindToolResult,
			Content: content, Meta: string(meta), At: time.Now().Format(time.RFC3339Nano),
		}, j.trace)
	}
	return true, nil
}

// reportPhaseOf 把 owner 的补报 phase 翻成 memory 账本认的 phase（S-001 §3.9 词汇）。
// ok=false 表示词汇不认识或 phase/final 不自洽（例如 done 却没标 final）：不写账本。
func reportPhaseOf(ownerPhase string, final bool) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(ownerPhase)) {
	case "queued", "accepted":
		if final {
			return "", false
		}
		return "accepted", true
	case "approved":
		if final {
			return "", false
		}
		return "approved", true
	case "rejected":
		if !final {
			return "", false
		}
		return "failed", true
	case "done":
		if !final {
			return "", false
		}
		return "done", true
	case "failed":
		if !final {
			return "", false
		}
		return "failed", true
	case "timeout":
		if !final {
			return "", false
		}
		return "timeout", true
	// timer 用 "canceled"（一个 l），S-001 词汇是 "cancelled"（两个 l）；同一件事一个落点。
	case "canceled", "cancelled":
		if !final {
			return "", false
		}
		return "cancelled", true
	}
	return "", false
}

// reportFingerprint 是「同一份 owner 报告」的稳定指纹：item + phase + final + text + data。
// 重试（同一份内容）复用同一 report_id/seq；内容不同才是新报告，拿新 seq。
func reportFingerprint(item string, p protocol.AsyncResultPayload) string {
	h := sha256.New()
	for _, part := range [][]byte{
		[]byte(item), []byte(p.Phase), []byte(strconv.FormatBool(p.Final)), []byte(p.Text), p.Data,
	} {
		_, _ = h.Write(part)
		_, _ = h.Write([]byte{0}) // 分隔符：避免 "ab"+"c" 与 "a"+"bc" 撞同一指纹
	}
	return hex.EncodeToString(h.Sum(nil))
}

// nextReport 为一份新报告取 (report_id, seq)；同一指纹重试回同一对值。
// 调用方需持有 job.deliveryMu（受理补报发生在 job 发布到 asyncJobs 之前，同样安全）。
func (j *asyncJob) nextReport(key string) (string, int64) {
	if j.reports == nil {
		j.reports = map[string]asyncReportKey{}
	}
	if r, ok := j.reports[key]; ok {
		return r.id, r.seq
	}
	j.reportSeq++
	r := asyncReportKey{id: spv2.NewID(), seq: j.reportSeq}
	j.reports[key] = r
	return r.id, r.seq
}

// recordCallFailure 是 nil 安全的指标累加（未 Start 的骨架没有 Registry）。
func (s *Server) recordCallFailure(metric, service, phase string) {
	reg := s.metricsRegistry()
	if reg == nil {
		return
	}
	labels := map[string]string{"service": service}
	if phase != "" {
		labels["phase"] = phase
	}
	reg.Counter(metric, "调用账本接线失败", labels, 1)
}
