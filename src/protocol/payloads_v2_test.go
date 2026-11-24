package protocol

// 记忆中心 v2（S-001）载荷的线上契约测试：
//   - 每个新结构体 MessagePack 往返一次，字段不丢；
//   - 线上键名必须是 S-001 词汇表里的 snake_case（结构体标签与 JSON 名一致）；
//   - omitempty 的可选字段为空时不得上线；
//   - 未知字段必须被忽略（协议只增不改：老读者读到新字段要活着）；
//   - StoreReport.Data 是 msgpack.RawMessage 透传，字节不得被改写。

import (
	"bytes"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// v2SnakeKeyRE 是线上键名的唯一形状：小写字母开头、下划线分段。
var v2SnakeKeyRE = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// mustEncodeV2 编码失败即测试失败（用例里的值都是可编码的）。
func mustEncodeV2(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := EncodePayload(v)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	return raw
}

// decodeV2 把线路字节解到 map，用于断言键名与 omitempty。
func decodeV2(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := DecodeRaw(raw, &m); err != nil {
		t.Fatalf("解码为 map 失败: %v", err)
	}
	return m
}

// checkV2SnakeKeys 递归检查所有对象键名都是 snake_case（含嵌套对象与对象数组）。
func checkV2SnakeKeys(t *testing.T, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if !v2SnakeKeyRE.MatchString(k) {
				t.Fatalf("%s 下的键 %q 不是 snake_case", path, k)
			}
			next := path + "." + k
			if path == "" {
				next = k
			}
			checkV2SnakeKeys(t, next, child)
		}
	case []any:
		for _, child := range x {
			checkV2SnakeKeys(t, path+"[]", child)
		}
	}
}

// assertV2ExactKeys 断言顶层键集合**恰好**是 want（多键少键都算契约漂移），并递归检查 snake_case。
func assertV2ExactKeys(t *testing.T, raw []byte, want ...string) map[string]any {
	t.Helper()
	m := decodeV2(t, raw)
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for k := range m {
		if !wantSet[k] {
			t.Fatalf("线上多出键 %q（不在 S-001 期望集合里）: 实际键=%v", k, keysOfV2(m))
		}
	}
	for k := range wantSet {
		if _, ok := m[k]; !ok {
			t.Fatalf("线上缺少键 %q: 实际键=%v", k, keysOfV2(m))
		}
	}
	checkV2SnakeKeys(t, "", m)
	return m
}

// assertV2NestedKeys 断言某个顶层键的值（对象）恰好包含 want 这些键。
func assertV2NestedKeys(t *testing.T, m map[string]any, parent string, want ...string) {
	t.Helper()
	raw, ok := m[parent]
	if !ok {
		t.Fatalf("缺少顶层键 %q", parent)
	}
	nested, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("键 %q 不是对象: %T", parent, raw)
	}
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for k := range nested {
		if !wantSet[k] {
			t.Fatalf("%q 下多出键 %q: 实际键=%v", parent, k, keysOfV2(nested))
		}
	}
	for k := range wantSet {
		if _, ok := nested[k]; !ok {
			t.Fatalf("%q 下缺少键 %q: 实际键=%v", parent, k, keysOfV2(nested))
		}
	}
}

func keysOfV2(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestPayloadsV2RoundTrip 每个新结构体（含两个既有结构体新增字段）至少一条往返。
func TestPayloadsV2RoundTrip(t *testing.T) {
	reportData := mustEncodeV2(t, map[string]any{"exit_code": int64(3), "stdout": "ok"})
	cases := []struct {
		name string
		in   any
	}{
		{"StoreReport", StoreReport{
			SourceService: "shell", ReportID: "01J-report", ReportSeq: 3, CallID: "01J-call",
			Phase: "done", Final: true, Data: reportData,
		}},
		{"StoreResult", StoreResult{ItemID: "01J-item", Seq: 123, Deduped: true}},
		{"StorePayload", StorePayload{
			ConversationID: "work-1", Boss: "web", Role: "tool", Content: "done",
			Kind: "tool_result", Meta: `{"service":"shell"}`, Seq: 9, TaskID: "task-1", TaskName: "修序号",
			ClientItemID: "report:shell:01J-report", Visibility: "task",
			Report: &StoreReport{SourceService: "shell", ReportID: "01J-report", ReportSeq: 3, CallID: "01J-call", Phase: "done"},
		}},
		{"AsyncResultPayload", AsyncResultPayload{
			ItemID: "01J-call", Phase: "done", Text: "执行完成", Data: reportData, Final: true,
			ReportID: "01J-report", ReportSeq: 3, CallID: "01J-call",
		}},
		{"Trigger", Trigger{Kind: "tool_result", Item: "01J-item"}},
		{"Degraded", Degraded{
			Level: "warn", Reason: "记忆服务写入慢", Since: "2026-11-24T17:00:00Z",
			RepairHint: "检查 data/memory.db 所在磁盘",
		}},
		{"PendingRef", PendingRef{Call: "01J-call", Observe: "waiting", Exec: "running"}},
		{"TaskBlocker", TaskBlocker{Work: "work-1", Call: "01J-call", Reason: "子 work 卡死"}},
		{"LLMToolCall", LLMToolCall{ID: "call-1", Name: "shell__run", Arguments: `{"command":"go test"}`}},
		{"LLMMessage", LLMMessage{
			Role: "assistant", Content: "先跑测试", ToolCalls: []LLMToolCall{{
				ID: "call-1", Name: "shell__run", Arguments: `{"command":"go test"}`,
			}},
			ToolCallID: "call-1", Name: "shell__run",
		}},
		{"WorkView", WorkView{
			ID: "work-1", Task: "task-1", Class: "request", Status: "active",
			Owner: "web:private:owner", Agent: "agent-1", Entry: "web", Goal: "修好序号",
			CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:01:00Z",
		}},
		{"ContextRequest", ContextRequest{
			Work: "work-1", Trigger: Trigger{Kind: "user", Item: "01J-item"}, Schema: "context/2",
		}},
		{"ContextResponse", ContextResponse{
			Schema: "context/2", SnapshotID: "01J-snap", ExpiresAt: "2026-11-25T17:00:00Z",
			Messages: []LLMMessage{
				{Role: "system", Content: "你是 SUMP 的 agent"},
				{Role: "user", Content: "你好"},
			},
			Task: Task{
				ID: "task-1", ConversationID: "conv-1", Name: "修序号", Status: "active",
				CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:01:00Z",
			},
			Work: WorkView{
				ID: "work-1", Task: "task-1", Class: "request", Status: "active",
				Owner: "web:private:owner", Agent: "agent-1", Entry: "web", Goal: "修好序号",
				CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:01:00Z",
			},
			Degraded:     &Degraded{Level: "warn", Reason: "缓存不可用"},
			Warnings:     []string{"上下文被截断"},
			Pending:      []PendingRef{{Call: "01J-call", Observe: "waiting", Exec: "running"}},
			TaskBlockers: []TaskBlocker{{Work: "work-2", Reason: "子 work 卡死"}},
			AssembledAt:  "2026-11-24T17:02:00Z",
		}},
		{"TriggerCompletePayload", TriggerCompletePayload{
			Work: "work-1", Trigger: Trigger{Kind: "tool_result", Item: "01J-item"},
		}},
		{"TriggerCompleteResult", TriggerCompleteResult{Completed: true}},
		{"CallOpenPayload", CallOpenPayload{
			Work: "work-1", Service: "shell", Action: "run", Mode: "once",
			Pausable: true, TTLMS: 60000, ExecDeadlineMS: 300000,
		}},
		{"CallOpenResult", CallOpenResult{CallID: "01J-call"}},
		{"CallControlPayload", CallControlPayload{
			ID: "01J-call", ControlID: "01J-control", Op: "pause", TTLMS: 60000,
		}},
		{"CallControlResult", CallControlResult{
			ID: "01J-call", ControlID: "01J-control", Observe: "not_waiting", Exec: "unknown",
			Reason: "owner 未确认暂停",
		}},
		{"CallStatusPayload", CallStatusPayload{IDs: []string{"01J-call-1", "01J-call-2"}}},
		{"CallStatusItem", CallStatusItem{
			ID: "01J-call", Observe: "waiting", Exec: "running", Phase: "done",
			OwnerService: "shell", UpdatedAt: "2026-11-24T17:02:00Z",
		}},
		{"CallStatusResult", CallStatusResult{
			OwnerRecoveryComplete: true,
			Items:                 []CallStatusItem{{ID: "01J-call", Observe: "waiting", Exec: "running"}},
		}},
		{"ResidentItem", ResidentItem{
			ID: "01J-call", Kind: "timer", Title: "每 30s 对账", Observe: "waiting", Exec: "running",
			Phase: "unknown", OwnerService: "timer", OriginSession: "sess-opaque-1",
			CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:02:00Z",
		}},
		{"ResidentListPayload", ResidentListPayload{Limit: 50, Cursor: "cursor-1"}},
		{"ResidentListResult", ResidentListResult{
			Items: []ResidentItem{{ID: "01J-call", Kind: "timer", Observe: "waiting", Exec: "running"}},
			Next:  "cursor-2",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustEncodeV2(t, tc.in)
			out := reflect.New(reflect.TypeOf(tc.in))
			if err := DecodeRaw(raw, out.Interface()); err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			got := out.Elem().Interface()
			if !reflect.DeepEqual(tc.in, got) {
				t.Fatalf("往返后字段不符:\n in=%+v\nout=%+v", tc.in, got)
			}
		})
	}
}

// TestPayloadsV2WireNames 钉住线上键名：全部字段非零时，顶层键集合必须与 S-001 完全一致。
func TestPayloadsV2WireNames(t *testing.T) {
	reportData := mustEncodeV2(t, map[string]any{"exit_code": int64(3)})
	cases := []struct {
		name   string
		in     any
		want   []string
		nested map[string][]string
	}{
		{
			name: "StorePayload",
			in: StorePayload{
				ConversationID: "work-1", Boss: "web", Role: "tool", Content: "done",
				Kind: "tool_result", Meta: "{}", Seq: 9, TaskID: "task-1", TaskName: "修序号",
				ClientItemID: "report:shell:01J-report", Visibility: "task",
				Report: &StoreReport{SourceService: "shell", ReportID: "01J-report", ReportSeq: 3, CallID: "01J-call", Phase: "done", Data: reportData},
			},
			want: []string{
				"conversation_id", "boss", "role", "content", "kind", "meta", "seq",
				"task_id", "task_name", "client_item_id", "visibility", "report",
			},
			nested: map[string][]string{
				"report": {"source_service", "report_id", "report_seq", "call_id", "phase", "data"},
			},
		},
		{
			name: "StoreReport",
			in: StoreReport{
				SourceService: "shell", ReportID: "01J-report", ReportSeq: 3, CallID: "01J-call",
				Phase: "done", Final: true, Data: reportData,
			},
			want: []string{"source_service", "report_id", "report_seq", "call_id", "phase", "final", "data"},
		},
		{
			name: "StoreResult",
			in:   StoreResult{ItemID: "01J-item", Seq: 123, Deduped: true},
			want: []string{"item_id", "seq", "deduped"},
		},
		{
			name: "AsyncResultPayload",
			in: AsyncResultPayload{
				ItemID: "01J-call", Phase: "done", Text: "完成", Data: reportData, Final: true,
				ReportID: "01J-report", ReportSeq: 3, CallID: "01J-call",
			},
			want: []string{"item_id", "phase", "text", "data", "final", "report_id", "report_seq", "call_id"},
		},
		{
			name: "Trigger",
			in:   Trigger{Kind: "user", Item: "01J-item"},
			want: []string{"kind", "item"},
		},
		{
			name: "Degraded",
			in: Degraded{
				Level: "warn", Reason: "写入慢", Since: "2026-11-24T17:00:00Z", RepairHint: "查磁盘",
			},
			want: []string{"level", "reason", "since", "repair_hint"},
		},
		{
			name: "PendingRef",
			in:   PendingRef{Call: "01J-call", Observe: "waiting", Exec: "running"},
			want: []string{"call", "observe", "exec"},
		},
		{
			name: "TaskBlocker",
			in:   TaskBlocker{Work: "work-1", Call: "01J-call", Reason: "卡死"},
			want: []string{"work", "call", "reason"},
		},
		{
			name: "LLMToolCall",
			in:   LLMToolCall{ID: "call-1", Name: "shell__run", Arguments: "{}"},
			want: []string{"id", "name", "arguments"},
		},
		{
			name: "LLMMessage",
			in: LLMMessage{
				Role: "assistant", Content: "跑测试", ToolCalls: []LLMToolCall{{ID: "call-1", Name: "shell__run", Arguments: "{}"}},
				ToolCallID: "call-1", Name: "shell__run",
			},
			want: []string{"role", "content", "tool_calls", "tool_call_id", "name"},
		},
		{
			name: "WorkView",
			in: WorkView{
				ID: "work-1", Task: "task-1", Class: "request", Status: "active",
				Owner: "web:owner", Agent: "agent-1", Entry: "web", Goal: "修序号",
				CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:01:00Z",
			},
			want: []string{
				"id", "task", "class", "status", "owner", "agent", "entry", "goal",
				"created_at", "updated_at",
			},
		},
		{
			name: "ContextRequest",
			in:   ContextRequest{Work: "work-1", Trigger: Trigger{Kind: "user", Item: "01J-item"}, Schema: "context/2"},
			want: []string{"work", "trigger", "schema"},
			nested: map[string][]string{
				"trigger": {"kind", "item"},
			},
		},
		{
			name: "ContextResponse",
			in: ContextResponse{
				Schema: "context/2", SnapshotID: "01J-snap", ExpiresAt: "2026-11-25T17:00:00Z",
				Messages: []LLMMessage{{Role: "user", Content: "你好", ToolCalls: []LLMToolCall{{ID: "c1", Name: "shell__run", Arguments: "{}"}}, ToolCallID: "c1", Name: "shell__run"}},
				Task: Task{ID: "task-1", ConversationID: "conv-1", Name: "修序号", Status: "active",
					CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:01:00Z"},
				Work: WorkView{ID: "work-1", Task: "task-1", Class: "request", Status: "active",
					Owner: "web:owner", Agent: "agent-1", Entry: "web", Goal: "修序号",
					CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:01:00Z"},
				Degraded:     &Degraded{Level: "warn", Reason: "缓存不可用", Since: "s", RepairHint: "h"},
				Warnings:     []string{"截断"},
				Pending:      []PendingRef{{Call: "01J-call", Observe: "waiting", Exec: "running"}},
				TaskBlockers: []TaskBlocker{{Work: "work-2", Call: "01J-call", Reason: "卡死"}},
				AssembledAt:  "2026-11-24T17:02:00Z",
			},
			want: []string{
				"schema", "snapshot_id", "expires_at", "messages", "task", "work",
				"degraded", "warnings", "pending", "task_blockers", "assembled_at",
			},
			nested: map[string][]string{
				"task": {"id", "conversation_id", "name", "status", "created_at", "updated_at"},
				"work": {"id", "task", "class", "status", "owner", "agent", "entry", "goal", "created_at", "updated_at"},
			},
		},
		{
			name: "TriggerCompletePayload",
			in:   TriggerCompletePayload{Work: "work-1", Trigger: Trigger{Kind: "tool_result", Item: "01J-item"}},
			want: []string{"work", "trigger"},
			nested: map[string][]string{
				"trigger": {"kind", "item"},
			},
		},
		{
			name: "TriggerCompleteResult",
			in:   TriggerCompleteResult{Completed: true},
			want: []string{"completed"},
		},
		{
			name: "CallOpenPayload",
			in: CallOpenPayload{
				Work: "work-1", Service: "shell", Action: "run", Mode: "once",
				Pausable: true, TTLMS: 60000, ExecDeadlineMS: 300000,
			},
			want: []string{"work", "service", "action", "mode", "pausable", "ttl_ms", "exec_deadline_ms"},
		},
		{
			name: "CallOpenResult",
			in:   CallOpenResult{CallID: "01J-call"},
			want: []string{"call_id"},
		},
		{
			name: "CallControlPayload",
			in:   CallControlPayload{ID: "01J-call", ControlID: "01J-control", Op: "pause", TTLMS: 60000},
			want: []string{"id", "control_id", "op", "ttl_ms"},
		},
		{
			name: "CallControlResult",
			in:   CallControlResult{ID: "01J-call", ControlID: "01J-control", Observe: "not_waiting", Exec: "unknown", Reason: "未确认"},
			want: []string{"id", "control_id", "observe", "exec", "reason"},
		},
		{
			name: "CallStatusPayload",
			in:   CallStatusPayload{IDs: []string{"01J-call-1"}},
			want: []string{"ids"},
		},
		{
			name: "CallStatusItem",
			in: CallStatusItem{
				ID: "01J-call", Observe: "waiting", Exec: "running", Phase: "done",
				OwnerService: "shell", UpdatedAt: "2026-11-24T17:02:00Z",
			},
			want: []string{"id", "observe", "exec", "phase", "owner_service", "updated_at"},
		},
		{
			name: "CallStatusResult",
			in: CallStatusResult{
				OwnerRecoveryComplete: true,
				Items:                 []CallStatusItem{{ID: "01J-call", Observe: "waiting", Exec: "running"}},
			},
			want: []string{"owner_recovery_complete", "items"},
		},
		{
			name: "ResidentItem",
			in: ResidentItem{
				ID: "01J-call", Kind: "timer", Title: "对账", Observe: "waiting", Exec: "running",
				Phase: "unknown", OwnerService: "timer", OriginSession: "sess-1",
				CreatedAt: "2026-11-24T17:00:00Z", UpdatedAt: "2026-11-24T17:02:00Z",
			},
			want: []string{
				"id", "kind", "title", "observe", "exec", "phase", "owner_service",
				"origin_session", "created_at", "updated_at",
			},
		},
		{
			name: "ResidentListPayload",
			in:   ResidentListPayload{Limit: 50, Cursor: "cursor-1"},
			want: []string{"limit", "cursor"},
		},
		{
			name: "ResidentListResult",
			in: ResidentListResult{
				Items: []ResidentItem{{ID: "01J-call", Kind: "timer", Observe: "waiting", Exec: "running"}},
				Next:  "cursor-2",
			},
			want: []string{"items", "next"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := assertV2ExactKeys(t, mustEncodeV2(t, tc.in), tc.want...)
			for parent, keys := range tc.nested {
				assertV2NestedKeys(t, m, parent, keys...)
			}
		})
	}
}

// TestPayloadsV2OmitEmpty 空可选字段不得出现在线上（S-001 里标“可选 / 默认”的字段）。
func TestPayloadsV2OmitEmpty(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		absent []string
	}{
		{"StorePayload", StorePayload{ConversationID: "work-1", Role: "user", Content: "你好"},
			[]string{"client_item_id", "visibility", "report"}},
		{"AsyncResultPayload", AsyncResultPayload{ItemID: "01J-call", Phase: "done"},
			[]string{"report_id", "report_seq", "call_id", "text", "data", "final"}},
		{"StoreReport", StoreReport{SourceService: "shell", ReportID: "01J-report", ReportSeq: 1, CallID: "01J-call"},
			[]string{"phase", "final", "data"}},
		{"StoreResult", StoreResult{ItemID: "01J-item", Seq: 2}, []string{"deduped"}},
		{"Degraded", Degraded{Level: "warn", Reason: "写入慢"}, []string{"since", "repair_hint"}},
		{"TaskBlocker", TaskBlocker{Work: "work-1", Reason: "子 work 卡死"}, []string{"call"}},
		{"WorkView", WorkView{
			ID: "work-1", Task: "task-1", Class: "request", Status: "active",
			Owner: "web:owner", Agent: "agent-1", CreatedAt: "a", UpdatedAt: "b",
		}, []string{"entry", "goal"}},
		{"ContextRequest", ContextRequest{Work: "work-1", Trigger: Trigger{Kind: "user", Item: "01J-item"}},
			[]string{"schema"}},
		{"ContextResponse", ContextResponse{
			Schema: "context/2", SnapshotID: "01J-snap", ExpiresAt: "e",
			Messages: []LLMMessage{}, Task: Task{}, Work: WorkView{}, AssembledAt: "a",
		}, []string{"degraded", "warnings", "pending", "task_blockers"}},
		{"CallOpenPayload", CallOpenPayload{Work: "work-1", Service: "shell", Action: "run", Mode: "once"},
			[]string{"pausable", "ttl_ms", "exec_deadline_ms"}},
		{"CallControlPayload", CallControlPayload{ID: "01J-call", ControlID: "01J-control", Op: "pause"},
			[]string{"ttl_ms"}},
		{"CallControlResult", CallControlResult{ID: "01J-call", ControlID: "01J-control", Observe: "waiting", Exec: "running"},
			[]string{"reason"}},
		{"CallStatusItem", CallStatusItem{ID: "01J-call"},
			[]string{"observe", "exec", "phase", "owner_service", "updated_at"}},
		{"ResidentItem", ResidentItem{
			ID: "01J-call", Kind: "timer", Observe: "waiting", Exec: "running",
			OwnerService: "timer", CreatedAt: "a", UpdatedAt: "b",
		}, []string{"title", "phase", "origin_session"}},
		{"ResidentListPayload", ResidentListPayload{}, []string{"limit", "cursor"}},
		{"ResidentListResult", ResidentListResult{Items: []ResidentItem{}}, []string{"next"}},
		{"PendingRef", PendingRef{}, []string{"call", "observe", "exec"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := decodeV2(t, mustEncodeV2(t, tc.in))
			for _, k := range tc.absent {
				if v, ok := m[k]; ok {
					t.Fatalf("空可选字段 %q 不该上线（值为 %v）", k, v)
				}
			}
		})
	}
}

// TestPayloadsV2UnknownFieldsIgnored 协议只增不改：老读者读到新字段必须忽略而不是报错。
func TestPayloadsV2UnknownFieldsIgnored(t *testing.T) {
	t.Run("ContextResponse", func(t *testing.T) {
		raw := mustEncodeV2(t, map[string]any{
			"schema":       "context/2",
			"snapshot_id":  "01J-snap",
			"expires_at":   "2026-11-25T17:00:00Z",
			"assembled_at": "2026-11-24T17:02:00Z",
			"messages": []any{
				map[string]any{"role": "user", "content": "你好", "future_message_field": true},
			},
			"task":        map[string]any{"id": "task-1", "future_task_field": int64(1)},
			"work":        map[string]any{"id": "work-1", "future_work_field": "x"},
			"future_top":  []any{int64(1), int64(2)},
			"unknown_obj": map[string]any{"a": int64(1)},
		})
		var out ContextResponse
		if err := DecodeRaw(raw, &out); err != nil {
			t.Fatalf("未知字段不该导致解码失败: %v", err)
		}
		if out.Schema != "context/2" || out.SnapshotID != "01J-snap" || out.AssembledAt != "2026-11-24T17:02:00Z" {
			t.Fatalf("已知字段丢了: %+v", out)
		}
		if len(out.Messages) != 1 || out.Messages[0].Role != "user" || out.Messages[0].Content != "你好" {
			t.Fatalf("嵌套消息解析不符: %+v", out.Messages)
		}
		if out.Task.ID != "task-1" || out.Work.ID != "work-1" {
			t.Fatalf("嵌套对象解析不符: task=%+v work=%+v", out.Task, out.Work)
		}
	})

	t.Run("StorePayload", func(t *testing.T) {
		raw := mustEncodeV2(t, map[string]any{
			"conversation_id": "work-1",
			"role":            "tool",
			"content":         "done",
			"client_item_id":  "report:shell:01J-report",
			"visibility":      "task",
			"report": map[string]any{
				"source_service": "shell", "report_id": "01J-report", "report_seq": int64(3),
				"call_id": "01J-call", "phase": "done", "final": true,
				"future_report_field": "ignored",
			},
			"future_store_field": map[string]any{"deep": true},
		})
		var out StorePayload
		if err := DecodeRaw(raw, &out); err != nil {
			t.Fatalf("未知字段不该导致解码失败: %v", err)
		}
		if out.ClientItemID != "report:shell:01J-report" || out.Visibility != "task" {
			t.Fatalf("新增可选字段丢了: %+v", out)
		}
		if out.Report == nil || out.Report.SourceService != "shell" || out.Report.ReportSeq != 3 ||
			out.Report.CallID != "01J-call" || !out.Report.Final {
			t.Fatalf("嵌套 report 解析不符: %+v", out.Report)
		}
	})

	t.Run("AsyncResultPayload", func(t *testing.T) {
		raw := mustEncodeV2(t, map[string]any{
			"item_id": "01J-call", "phase": "done", "report_id": "01J-report",
			"report_seq": int64(3), "call_id": "01J-call", "future_field": "ignored",
		})
		var out AsyncResultPayload
		if err := DecodeRaw(raw, &out); err != nil {
			t.Fatalf("未知字段不该导致解码失败: %v", err)
		}
		if out.ReportID != "01J-report" || out.ReportSeq != 3 || out.CallID != "01J-call" {
			t.Fatalf("补报元数据丢了: %+v", out)
		}
	})

	t.Run("CallStatusResult", func(t *testing.T) {
		raw := mustEncodeV2(t, map[string]any{
			"owner_recovery_complete": true,
			"items": []any{
				map[string]any{"id": "01J-call", "exec": "unknown", "future_item_field": int64(1)},
			},
			"future_top": "ignored",
		})
		var out CallStatusResult
		if err := DecodeRaw(raw, &out); err != nil {
			t.Fatalf("未知字段不该导致解码失败: %v", err)
		}
		if !out.OwnerRecoveryComplete || len(out.Items) != 1 || out.Items[0].Exec != "unknown" {
			t.Fatalf("对账结果解析不符: %+v", out)
		}
	})
}

// TestStoreReportDataPassthrough 钉住 Data 是**透传**：
// owner 的 msgpack 字节原样上线，解码后逐字节相等，不被重新编码 / 解包 / 规范化。
func TestStoreReportDataPassthrough(t *testing.T) {
	inner := mustEncodeV2(t, map[string]any{
		"exit_code": int64(3),
		"stdout":    "ok",
		"nested":    map[string]any{"k": "v"},
	})
	in := StoreReport{
		SourceService: "shell", ReportID: "01J-report", ReportSeq: 3, CallID: "01J-call",
		Phase: "done", Final: true, Data: inner,
	}
	outer := mustEncodeV2(t, in)
	if !bytes.Contains(outer, inner) {
		t.Fatalf("Data 的 msgpack 字节没有原样出现在外层编码里（疑似被重新编码）:\n inner=%x\n outer=%x", inner, outer)
	}
	var out StoreReport
	if err := DecodeRaw(outer, &out); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if !bytes.Equal(out.Data, inner) {
		t.Fatalf("Data 透传后被改写:\n got=%x\nwant=%x", out.Data, inner)
	}
	// 语义仍然可解（透传不等于不可读）。
	var data map[string]any
	if err := DecodeRaw(out.Data, &data); err != nil {
		t.Fatalf("Data 自身应是合法 msgpack: %v", err)
	}
	if data["stdout"] != "ok" {
		t.Fatalf("Data 语义不符: %v", data)
	}
	// 重放路径（解码后再编码）字节也不得漂移。
	if again := mustEncodeV2(t, out); !bytes.Contains(again, inner) {
		t.Fatalf("重放编码后 Data 字节漂移了: %x", again)
	}
}

// TestPayloadsV2Tags 钉住“每个字段同时有 msgpack 与 json 标签，且二者同名”的约定。
func TestPayloadsV2Tags(t *testing.T) {
	fullTypes := []reflect.Type{
		reflect.TypeOf(StoreReport{}),
		reflect.TypeOf(StoreResult{}),
		reflect.TypeOf(Trigger{}),
		reflect.TypeOf(Degraded{}),
		reflect.TypeOf(PendingRef{}),
		reflect.TypeOf(TaskBlocker{}),
		reflect.TypeOf(LLMToolCall{}),
		reflect.TypeOf(LLMMessage{}),
		reflect.TypeOf(WorkView{}),
		reflect.TypeOf(ContextRequest{}),
		reflect.TypeOf(ContextResponse{}),
		reflect.TypeOf(TriggerCompletePayload{}),
		reflect.TypeOf(TriggerCompleteResult{}),
		reflect.TypeOf(CallOpenPayload{}),
		reflect.TypeOf(CallOpenResult{}),
		reflect.TypeOf(CallControlPayload{}),
		reflect.TypeOf(CallControlResult{}),
		reflect.TypeOf(CallStatusPayload{}),
		reflect.TypeOf(CallStatusItem{}),
		reflect.TypeOf(CallStatusResult{}),
		reflect.TypeOf(ResidentItem{}),
		reflect.TypeOf(ResidentListPayload{}),
		reflect.TypeOf(ResidentListResult{}),
	}
	checkFields := func(typ reflect.Type, names []string) {
		t.Helper()
		fields := make(map[string]reflect.StructField, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			fields[f.Name] = f
		}
		for _, name := range names {
			f, ok := fields[name]
			if !ok {
				t.Fatalf("%s 缺少字段 %s", typ.Name(), name)
			}
			mp := f.Tag.Get("msgpack")
			js := f.Tag.Get("json")
			if mp == "" || js == "" {
				t.Fatalf("%s.%s 必须同时有 msgpack 与 json 标签: mp=%q js=%q", typ.Name(), f.Name, mp, js)
			}
			mpName := strings.Split(mp, ",")[0]
			jsName := strings.Split(js, ",")[0]
			if mpName != jsName {
				t.Fatalf("%s.%s 的 msgpack/json 名不一致: %q vs %q", typ.Name(), f.Name, mpName, jsName)
			}
			if !v2SnakeKeyRE.MatchString(mpName) {
				t.Fatalf("%s.%s 的线上名 %q 不是 snake_case", typ.Name(), f.Name, mpName)
			}
		}
	}
	for _, typ := range fullTypes {
		names := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			names = append(names, typ.Field(i).Name)
		}
		checkFields(typ, names)
	}
	// 既有结构体是“只增不改”：只检查 v2 新增字段（老字段保持原样，不加 json 标签也不动它）。
	checkFields(reflect.TypeOf(StorePayload{}), []string{"ClientItemID", "Visibility", "Report"})
	checkFields(reflect.TypeOf(AsyncResultPayload{}), []string{"ReportID", "ReportSeq", "CallID"})
}
