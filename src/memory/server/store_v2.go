package server

// store 的 v2 路径：client_item_id 幂等与补报 receipt 接收（S-001 §3.0 / §4.3 / §5.4）。
//
// 旧路径（不带 client_item_id）完全不变，见 server.go 的 store；这里只处理新契约：
//
//  1. work 必填（只给 task_id 时用它兜底）——消息归属执行单元，不归属会话；
//  2. payload_hash = sha256(JCS(规范化载荷))，覆盖 kind / role / content / meta /
//     visibility 与 report 全字段（§3.0 规则 4）；
//  3. 先查 store_receipts：同键同 hash = 重放（回既有 item_id + seq、deduped=true），
//     同键不同 hash = conflict；
//  4. 未重放 → RecordV2 落库（写 work_id / visibility）→ 回填 receipt 的 item_id；
//  5. 补报路径额外写 report_receipts；同 report_id 同 hash 重放去重，换内容 / 换 ID
//     占同一 (call_id, report_seq) 都回 conflict（§6.2 交人工介入）。
//
// 注意：SPv2 原生入口（native.go 的 nativeStore）目前把所有 store 错误统一回
// invalid_argument；本文件返回的 conflict 要想在线上可见，需要那个包装保留 *spv2.Error
// 的错误码。本切片不改 native.go，仅在此标注衔接点。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/protocol/jcs"
	"github.com/NouSart/sump/spv2"
)

// storeV2Mu 串行化新路径的两阶段落库（占位 receipt → 落消息 → 回填 item_id）。
// 同一进程内并发的同键重放必须等前一阶段收尾，否则两个请求会各写一条消息。
var storeV2Mu sync.Mutex

// storeV2 是 store 的新路径：按 client_item_id 幂等入账，并把补报元数据落 report_receipts。
// 返回值是 protocol.StoreResult（item_id / seq / deduped），错误一律是 spv2.ErrCode 的错误。
func (s *Server) storeV2(p *protocol.StorePayload) (protocol.StoreResult, error) {
	// work 是消息的归属（S-001 §2.1）；TaskID 是老字段，只作兜底。
	work := strings.TrimSpace(p.Work)
	if work == "" {
		work = strings.TrimSpace(p.TaskID)
	}
	if work == "" {
		return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInvalidArgument,
			"store: work 必填（work 为空时用 task_id 兜底，两者都空）", false, nil)
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

	ctx := context.Background()
	// 同键请求在进程内串行：两阶段 receipt 之间不能让别的同键请求插进来。
	storeV2Mu.Lock()
	defer storeV2Mu.Unlock()

	// 第一阶段：查幂等键。不存在时插入占位行（item_id 为空，落库后回填）。
	existing, deduped, err := s.cfg.Store.PutStoreReceipt(ctx, work, p.ClientItemID, payloadHash, "")
	if err != nil {
		return protocol.StoreResult{}, storeReceiptError(work, p.ClientItemID, err)
	}
	if deduped {
		seq, err := s.cfg.Store.MessageByItemID(ctx, existing)
		if err != nil {
			return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInternal,
				"store: 幂等回执指向的消息读不到: "+err.Error(), false, nil)
		}
		// 补报的崩溃窗口（§5.4）：消息与 store receipt 已提交、report receipt 还没写就崩了。
		// 重放按同一 hash 补齐（已写过则命中 dedupe），冲突仍按 conflict 上报，不静默吞掉。
		if p.Report != nil {
			if _, err := s.cfg.Store.PutReportReceipt(ctx, work, p.Report.SourceService, p.Report.ReportID,
				payloadHash, existing, p.Report.CallID, p.Report.ReportSeq); err != nil {
				return protocol.StoreResult{}, storeReceiptError(work, p.Report.ReportID, err)
			}
		}
		return protocol.StoreResult{ItemID: existing, Seq: seq, Deduped: true}, nil
	}

	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	itemID, seq, _, err := s.cfg.Store.RecordV2(cid, p.Role, p.Content, p.Kind, p.Meta, p.Seq, p.TaskID, work, visibility)
	if err != nil {
		return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInternal, "store: 落库失败: "+err.Error(), false, nil)
	}
	// 第二阶段：把刚落的记录 id 回填进占位 receipt。
	existing, deduped, err = s.cfg.Store.PutStoreReceipt(ctx, work, p.ClientItemID, payloadHash, itemID)
	if err != nil {
		return protocol.StoreResult{}, storeReceiptError(work, p.ClientItemID, err)
	}
	if deduped && existing != "" && existing != itemID {
		// 极端情况下被重放抢先（如崩溃恢复路径上的并发）：以先落库的那条为准，
		// 本次多写的一行不影响幂等语义——重放必须回同一个 item_id 与 seq。
		seq, err := s.cfg.Store.MessageByItemID(ctx, existing)
		if err != nil {
			return protocol.StoreResult{}, spv2.ErrCode(spv2.CodeInternal,
				"store: 幂等回执指向的消息读不到: "+err.Error(), false, nil)
		}
		return protocol.StoreResult{ItemID: existing, Seq: seq, Deduped: true}, nil
	}

	// 补报路径：额外写 report_receipts（同 hash 重放 → deduped；冲突 → conflict）。
	reportDeduped := false
	if p.Report != nil {
		reportDeduped, err = s.cfg.Store.PutReportReceipt(ctx, work,
			p.Report.SourceService, p.Report.ReportID, payloadHash, itemID, p.Report.CallID, p.Report.ReportSeq)
		if err != nil {
			return protocol.StoreResult{}, storeReceiptError(work, p.Report.ReportID, err)
		}
	}
	return protocol.StoreResult{ItemID: itemID, Seq: seq, Deduped: reportDeduped}, nil
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

// storeReceiptError 把 store 包的幂等错误翻成 SPv2 错误码。
// ErrPayloadConflict 是唯一的业务冲突（同键不同载荷），其余按内部错误回。
func storeReceiptError(work, key string, err error) error {
	if errors.Is(err, store.ErrPayloadConflict) {
		return spv2.ErrCode(spv2.CodeConflict,
			fmt.Sprintf("store: 幂等键 %s/%s 已存在但载荷不同", work, key), false, nil)
	}
	return spv2.ErrCode(spv2.CodeInternal, "store: 幂等记录写入失败: "+err.Error(), false, nil)
}
