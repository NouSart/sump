package session

// store 的 v2 路径：client_item_id 幂等与补报 receipt 接收（S-001 §3.0 / §4.3 / §5.4）。
//
// 旧路径（不带 client_item_id）完全不变，见 actions.go 的 store；这里只处理新契约：
//
//  1. work 必填——消息归属执行单元，task_id 不能冒充 work；
//  2. payload_hash = sha256(JCS(规范化载荷))，覆盖 kind / role / content / meta /
//     visibility 与 report 全字段（§3.0 规则 4）；
//  3. store receipt、消息、report receipt 与 call 状态在同一个 SQLite 事务里落库；
//     响应 ACK 只在事务提交后返回（INV-13）；
//  4. 同键同 hash / 同 report_id 同 hash = 重放；同键不同 hash / 换 ID 占同一
//     (call_id, report_seq) = conflict；任何冲突都不得留下部分写入。
//
// 授权（R-023 P4）：走 v2 路径的每个请求都先在 authorizeTargetWork 里校验
// 「帧头 work == 载荷 work + 身份属于该 work」（owner / runtime 另有跨 work 通道），
// 匿名 from.id 一律拒绝。旧路径（ClientItemID 为空）不经过这里，行为不变。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/protocol/jcs"
	"github.com/NouSart/sump/spv2"
)

// storeV2 是 store 的新路径：按 client_item_id 幂等入账，并把补报元数据落 report_receipts。
// 返回值是 protocol.StoreResult（item_id / seq / deduped），错误一律是 spv2.ErrCode 的错误。
func (s *Server) storeV2(p *protocol.StorePayload, c caller) (protocol.StoreResult, error) {
	// work 是消息的归属（S-001 §2.1）：task 与 work 是两个标识空间，不能用 task_id 冒充 work。
	work := strings.TrimSpace(p.Work)
	if work == "" {
		return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInvalidArgument, "store: work 必填", false, nil)
	}
	// 先授权再算 hash / 落库：越权请求不产生任何写入或幂等回执。
	if _, err := s.authorizeTargetWork(context.Background(), work, c.work, "store", c.from); err != nil {
		return protocol.StoreResult{}, err
	}
	visibility := strings.TrimSpace(p.Visibility)
	if visibility == "" {
		visibility = "work" // 默认可见性：跨 work 读取默认不可见（INV-14）
	}
	switch visibility {
	case "work", "thread", "task":
	default:
		return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInvalidArgument,
			"store: visibility 只能是 work / thread / task，收到 "+visibility, false, nil)
	}
	payloadHash, err := storePayloadHash(p, visibility)
	if err != nil {
		return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInvalidArgument, "store: "+err.Error(), false, nil)
	}

	if err := validateStoreReport(p.Report); err != nil {
		return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInvalidArgument, "store: "+err.Error(), false, nil)
	}

	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	in := store.V2RecordInput{
		ConversationID: cid,
		Role:           p.Role,
		Kind:           p.Kind,
		Content:        p.Content,
		Meta:           p.Meta,
		TaskID:         p.TaskID,
		WorkID:         work,
		Visibility:     visibility,
		ClientItemID:   p.ClientItemID,
		PayloadHash:    payloadHash,
	}
	if p.Report != nil {
		in.Report = &store.V2ReportInput{
			SourceService: p.Report.SourceService,
			ReportID:      p.Report.ReportID,
			ReportSeq:     p.Report.ReportSeq,
			CallID:        p.Report.CallID,
			Phase:         p.Report.Phase,
			Final:         p.Report.Final,
			Data:          p.Report.Data,
		}
	}
	res, err := s.cfg.Store.RecordV2Atomic(context.Background(), in)
	if err != nil {
		return protocol.StoreResult{}, storeReceiptError(work, p.ClientItemID, err)
	}
	return protocol.StoreResult{ItemID: res.ItemID, Seq: res.Seq, Deduped: res.Deduped}, nil
}

// validateStoreReport 校验补报载荷的最小语义：幂等键、执行 call 与 phase 词汇必须明确。
// 终态报告必须 final=true；非终态 accepted/approved 只表示 owner 已受理或审批通过。
func validateStoreReport(r *protocol.StoreReport) error {
	if r == nil {
		return nil
	}
	if strings.TrimSpace(r.SourceService) == "" || strings.TrimSpace(r.ReportID) == "" || strings.TrimSpace(r.CallID) == "" {
		return errors.New("补报 report.source_service / report_id / call_id 必填")
	}
	if r.ReportSeq <= 0 {
		return fmt.Errorf("补报 report.report_seq 必须从 1 开始，收到 %d", r.ReportSeq)
	}
	switch r.Phase {
	case "accepted", "approved":
		if r.Final {
			return fmt.Errorf("补报 phase=%s 不能同时 final=true（终态只能是 done/failed/timeout/cancelled）", r.Phase)
		}
	case "done", "failed", "timeout", "cancelled":
		if !r.Final {
			return fmt.Errorf("补报 phase=%s 必须 final=true", r.Phase)
		}
	default:
		return fmt.Errorf("补报 phase %q 非法（accepted|approved|done|failed|timeout|cancelled）", r.Phase)
	}
	return nil
}

// storePayloadHash 计算 store 幂等用的 payload_hash（S-001 §3.0 规则 4）。
//
// 载荷形状与规范一致：{kind, role, content, meta, visibility, report?}；report 仅补报路径出现，
// 且覆盖 source_service / report_id / report_seq / call_id / phase / final / data 全字段。
// 先按 JCS 归一（对象键排序、数字最短形式），再 sha256——键序 / 空白 / 数字写法不同但语义
// 相同的载荷必须得到同一个 hash，否则重试会被误判成 conflict。
func storePayloadHash(p *protocol.StorePayload, visibility string) (string, error) {
	meta, err := storeMetaValue(p.Meta)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"kind":       p.Kind,
		"role":       p.Role,
		"content":    p.Content,
		"meta":       meta,
		"visibility": visibility,
	}
	if p.Report != nil {
		data, err := storeReportData(p.Report.Data)
		if err != nil {
			return "", err
		}
		payload["report"] = map[string]any{
			"source_service": p.Report.SourceService,
			"report_id":      p.Report.ReportID,
			"report_seq":     p.Report.ReportSeq,
			"call_id":        p.Report.CallID,
			"phase":          p.Report.Phase,
			"final":          p.Report.Final,
			"data":           data,
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("payload 无法规范化: %w", err)
	}
	return jcs.Hash(raw)
}

// storeMetaValue 把 meta（JSON 字符串）解析成参与 hash 的对象。
//
// 规则：
//   - 空串 / 空对象 = 没有附加信息，hash 里记 null（两种写法语义相同，不能因此 conflict）；
//   - 非法 JSON / 重复键 = invalid_argument（JCS 先校验，重复键不算「语义相同」）；
//   - 非对象（数组 / 标量）= invalid_argument（meta 的契约是对象）。
func storeMetaValue(raw string) (any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	canon, err := jcs.Canonicalize([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("meta 不是合法 JSON（或含重复键）: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(canon, &obj); err != nil {
		return nil, fmt.Errorf("meta 必须是 JSON 对象: %w", err)
	}
	if len(obj) == 0 {
		return nil, nil
	}
	return obj, nil
}

// storeReportData 把 report.data（owner 自定的 msgpack 透传字节）转成可参与 JCS 哈希的
// JSON 值。同一次补报的 outbox 重放带上来的字节相同，解出的值也相同，hash 因此稳定；
// 解不出来（非法 msgpack / 不可表示的值）按 invalid_argument 拒绝，不猜。
func storeReportData(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := protocol.DecodeRaw(raw, &v); err != nil {
		return nil, fmt.Errorf("report.data 不是合法 msgpack: %w", err)
	}
	// 先探一次 JSON 编码：JCS 只接受 JSON 值，msgpack 里可能有无法落成 JSON 的东西。
	if _, err := json.Marshal(v); err != nil {
		return nil, fmt.Errorf("report.data 无法规范化（存在 JCS 不支持的值）: %w", err)
	}
	return v, nil
}

// storeReceiptError 把 store 的幂等 / 账本错误翻成 SPv2 错误码。
//
// 三类都是业务结论，不是服务端故障：
//   - ErrPayloadConflict：同一个幂等键两次说了不一样的话（conflict，交人工处置）；
//   - ErrCallNotFound：补报引用的 call 本地没有（not_found，owner 该重发 call_open 侧的事实）；
//   - ErrCallConflict：补报与 call 账本对不上（work / service 不符或非法转移，conflict）。
func storeReceiptError(work, key string, err error) error {
	switch {
	case errors.Is(err, store.ErrPayloadConflict):
		return spv2.ErrCode(spv2.CodeConflict,
			fmt.Sprintf("store: 幂等键 %s/%s 已存在但载荷不同", work, key), false, nil)
	case errors.Is(err, store.ErrCallNotFound):
		return spv2.ErrCode(spv2.CodeNotFound, "store: 补报引用的调用不存在: "+err.Error(), false, nil)
	case errors.Is(err, store.ErrCallConflict):
		return spv2.ErrCode(spv2.CodeConflict, "store: 补报与调用账本冲突: "+err.Error(), false, nil)
	}
	return spv2.ErrCode(spv2.CodeInternal, "store: 幂等记录写入失败: "+err.Error(), false, nil)
}
