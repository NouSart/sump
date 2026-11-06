package spv2

// msgpack 是本模块**默认**的线上编解码器（见 codec.go 的 Marshal/Unmarshal、
// frame.go 的 ContentTypeMsgPack 默认值）。msgpack/v5 在字段没有 msgpack 标签时
// 退回 Go 字段名，于是同一份结构体在两种编解码器下会给出不同键名：
// json 说 tool_calls / deadline_at，线上却是 ToolCalls / DeadlineAt。
//
// 这里断言的是「线上真实键名」，不是 Go 结构体之间的自反往返——
// 漏了标签的话 Go→Go 照样对称、照样绿，所以那种写法测不出问题。

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// wireKeys 用包内默认编解码器编码 v，再把线上载荷解成 map 取键名。
// 值一律用 msgpack.RawMessage 接住：这样 time.Time 之类的特殊值不影响「键名」这件事。
func wireKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := msgpack.Marshal(v)
	if err != nil {
		t.Fatalf("msgpack.Marshal(%T): %v", v, err)
	}
	var onWire map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(raw, &onWire); err != nil {
		t.Fatalf("把 %T 的线上载荷解成 map: %v", v, err)
	}
	keys := make([]string, 0, len(onWire))
	for k := range onWire {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonKeys 取同一结构体的 JSON 键名（对外契约的写法来源）。
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	var onWire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &onWire); err != nil {
		t.Fatalf("把 %T 的 JSON 载荷解成 map: %v", v, err)
	}
	keys := make([]string, 0, len(onWire))
	for k := range onWire {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// wireKeyCases 是「每个结构体 = 一组非零值 + 期望的线上键名」。
// 字段全部填满，是为了不让 omitempty 把该出现的键名藏起来。
func wireKeyCases() []struct {
	name  string
	value any
	want  []string
} {
	return []struct {
		name  string
		value any
		want  []string
	}{
		{
			name:  "Charge（租约扣减）",
			value: Charge{ToolCalls: 1, Tokens: 2, Approvals: 3, Children: 4, Depth: 5},
			want:  []string{"approvals", "children", "depth", "tokens", "tool_calls"},
		},
		{
			name:  "Remaining（租约余额）",
			value: Remaining{DeadlineAt: "2026-11-06T00:00:00Z", ToolCalls: 1, Tokens: 2, Approvals: 3, Children: 4, Depth: 5},
			want:  []string{"approvals", "children", "deadline_at", "depth", "tokens", "tool_calls"},
		},
		{
			name: "Metric（指标样本）",
			value: Metric{
				Name: "sump_requests_total", Type: "histogram", Help: "Total requests", Unit: "1",
				Labels: map[string]string{"method": "echo"}, Value: 1, Count: 2, Sum: 3,
				Buckets: []Bucket{{LE: 0.5, Count: 7}},
			},
			want: []string{"buckets", "count", "help", "labels", "name", "sum", "type", "unit", "value"},
		},
		{
			name:  "Bucket（直方图桶）",
			value: Bucket{LE: 0.5, Count: 7},
			want:  []string{"count", "le"},
		},
		{
			name: "QueuedMessage（A2A 投递）",
			value: QueuedMessage{
				ID: "m1", WorkID: "w1", From: ServiceRef("a"), To: ServiceRef("b"),
				Method: "deliver", ReplyTo: "c1", Body: []byte("payload"), State: MessagePending,
				Attempts: 1, LastError: "boom", CreatedAt: time.Now().UTC(),
				LeaseUntil: time.Now().UTC().Add(time.Minute),
			},
			want: []string{"attempts", "body", "created_at", "from", "id", "last_error", "lease_until", "method", "reply_to", "state", "to", "work_id"},
		},
		{
			name: "Frame（线帧）",
			value: Frame{
				V: ProtocolVersion, ID: "f1", Type: TypeReply, Work: "w1",
				From: ServiceRef("svc"), To: AgentRef("a"), Method: "echo", ReplyTo: "c1",
				ContentType: ContentTypeMsgPack, Schema: "s1", IdempotencyKey: "k1",
				Stream: &Stream{ID: "s1", Seq: 1, End: true}, Status: StatusOK,
				Error: &Error{Code: CodeInternal, Message: "boom", Retryable: true},
				Body:  []byte("payload"),
			},
			// 帧头的键名是跨实现的契约（spec 的「能力可替换」靠它）：
			// 本包自己编自己解永远对称，只有把键名钉在字节上才测得出漂移。
			want: []string{"body", "content_type", "error", "from", "id", "idempotency_key",
				"method", "reply_to", "schema", "status", "stream", "to", "type", "v", "work"},
		},
		{
			name:  "SweepResult（清理计数）",
			value: SweepResult{Stalled: 1, Expired: 2, Retried: 3},
			want:  []string{"expired", "retried", "stalled"},
		},
	}
}

// TestMsgpackWireKeysAreSnakeCase 锁住「线上键名 = JSON 契约键名」。
// 删掉任一字段的 msgpack 标签，该键名立刻退回 Go 字段名（如 ToolCalls），本用例变红。
func TestMsgpackWireKeysAreSnakeCase(t *testing.T) {
	for _, tc := range wireKeyCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := wireKeys(t, tc.value)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("线上键名 = %v, want %v", got, tc.want)
			}
			for _, k := range got {
				if strings.IndexFunc(k, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0 {
					t.Fatalf("键名 %q 是 Go 字段名风格：该字段缺 msgpack 标签，msgpack/v5 退回了字段名", k)
				}
			}
		})
	}
}

// TestMsgpackAndJSONWireKeysAgree 是同一条规则的交叉版本：
// 同一结构体在两种编解码器下必须给出同一套键名（只写一个标签早晚会对不上）。
func TestMsgpackAndJSONWireKeysAgree(t *testing.T) {
	for _, tc := range wireKeyCases() {
		t.Run(tc.name, func(t *testing.T) {
			mp := wireKeys(t, tc.value)
			js := jsonKeys(t, tc.value)
			if !reflect.DeepEqual(mp, js) {
				t.Fatalf("两种编解码器键名不一致: msgpack=%v json=%v", mp, js)
			}
		})
	}
}

// TestMsgpackTaggedStructsRoundTrip 确认加了标签之后载荷仍读得回来（不是只改键名不改语义）。
func TestMsgpackTaggedStructsRoundTrip(t *testing.T) {
	want := QueuedMessage{
		ID: "m1", WorkID: "w1", From: HumanRef("p"), To: AgentRef("a"),
		Method: "deliver", ReplyTo: "c1", Body: []byte("payload"), State: MessageClaimed,
		Attempts: 2, LastError: "timeout", CreatedAt: time.Now().UTC().Truncate(time.Second),
		LeaseUntil: time.Now().UTC().Add(time.Minute).Truncate(time.Second),
	}
	raw, err := msgpack.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got QueuedMessage
	if err := msgpack.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// 时间字段按瞬时比较（编解码器可能保留不同的时区对象），其余字段逐值比较。
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.LeaseUntil.Equal(want.LeaseUntil) {
		t.Fatalf("时间往返不一致: got created=%v lease=%v, want created=%v lease=%v",
			got.CreatedAt, got.LeaseUntil, want.CreatedAt, want.LeaseUntil)
	}
	gotBody, wantBody := got, want
	gotBody.CreatedAt, gotBody.LeaseUntil = time.Time{}, time.Time{}
	wantBody.CreatedAt, wantBody.LeaseUntil = time.Time{}, time.Time{}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Fatalf("往返不一致:\n got = %+v\nwant = %+v", gotBody, wantBody)
	}

	charge := Charge{ToolCalls: 3, Tokens: 42, Approvals: 1, Children: 2, Depth: 4}
	raw, err = msgpack.Marshal(charge)
	if err != nil {
		t.Fatalf("Marshal(Charge): %v", err)
	}
	// 用裸 map 读，证明对端按契约键名就能取到值——不依赖本包的 Go 结构体。
	var flat map[string]any
	if err := msgpack.Unmarshal(raw, &flat); err != nil {
		t.Fatalf("Unmarshal(Charge 到 map): %v", err)
	}
	if s := fmt.Sprint(flat["tool_calls"]); s != "3" {
		t.Fatalf("线上 tool_calls = %#v, want 3", flat["tool_calls"])
	}
	if _, ok := flat["ToolCalls"]; ok {
		t.Fatal("线上仍出现 Go 字段名 ToolCalls")
	}
}
