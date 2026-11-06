package spv2

// 本文件把 validate.go 里每一条「没有测试覆盖的 guard」钉住：
// 删掉对应 guard，相应用例必须变红（这些用例是逐条对着源码写的）。
//
// 为什么帧用结构体字面量而不是构造器来造：
// 构造器（NewCall / NewReply / NewStreamFrame…）自己会写 V: ProtocolVersion，
// 还会顺手补齐字段，非法版本 / 非法字段组合根本造不出来，只能直接搭 Frame。
//
// 钉住的规则（AGENTS.md「不可违反的技术约定」）：
//   - 只有一种线协议：SPv2.1 帧（v = "spv2.1"，type = call/reply/event/stream）
//   - 未知 type 的帧必须被拒绝，不得静默接受
//   - 结果归属逐跳自定、不可丢失：from/to/authority 的取值空间是闭合的
//   - method 只增不改：reply/stream 的排他字段组合属于契约，不能放宽

import (
	"strings"
	"testing"
)

// guardCall 造一帧「除被测字段外都合法」的 call，便于逐个 guard 做变异。
func guardCall(from, to Ref) Frame {
	return Frame{
		V: ProtocolVersion, ID: "f1", Type: TypeCall, Work: "w1",
		From: from, To: to, Method: "m", ContentType: ContentTypeMsgPack,
	}
}

// guardReply 造一帧合法的 reply（状态 ok、无 error），供 reply 排他字段用例变异。
func guardReply() Frame {
	return Frame{
		V: ProtocolVersion, ID: "r1", Type: TypeReply, Work: "w1",
		From: ServiceRef("s"), To: AgentRef("caller"), ReplyTo: "c1",
		Status: StatusOK, ContentType: ContentTypeMsgPack,
	}
}

// guardStream 造一帧合法的非终态 stream，供 stream 边界用例变异。
func guardStream() Frame {
	return Frame{
		V: ProtocolVersion, ID: "st1", Type: TypeStream, Work: "w1",
		From: ServiceRef("s"), To: AgentRef("caller"), ReplyTo: "c1",
		Stream: &Stream{ID: "s1", Seq: 1}, ContentType: ContentTypeMsgPack,
	}
}

// TestValidateFrameGuards 覆盖帧结构 guard：版本、type 白名单、reply/stream 排他字段。
// 用例故意把 wantMsg 写成 guard 原文片段：guard 被删掉时得到 nil，
// 被改写时得到不同信息，两种都会在这里变红。
func TestValidateFrameGuards(t *testing.T) {
	cases := []struct {
		name    string
		rule    string
		frame   func() Frame
		wantMsg string
	}{
		{
			name:    "版本不是 spv2.1 必须拒绝",
			rule:    "只有一种线协议：SPv2.1 帧（v: \"spv2.1\"）",
			frame:   func() Frame { f := guardCall(HumanRef("p"), ServiceRef("s")); f.V = "spv1"; return f },
			wantMsg: "v 必须是",
		},
		{
			name:    "版本为空也非法（Normalize 之前）",
			rule:    "版本是必填帧头，缺失只能在 Normalize 里被补成当前版本，Validate 本身不接受空值",
			frame:   func() Frame { f := guardCall(HumanRef("p"), ServiceRef("s")); f.V = ""; return f },
			wantMsg: "v 必须是",
		},
		{
			name:    "未知 type 必须拒绝",
			rule:    "type 只有 call/reply/event/stream 四种",
			frame:   func() Frame { f := guardCall(HumanRef("p"), ServiceRef("s")); f.Type = "notification"; return f },
			wantMsg: "type 非法",
		},
		{
			name:    "空 type 必须拒绝",
			rule:    "type 只有 call/reply/event/stream 四种",
			frame:   func() Frame { f := guardCall(HumanRef("p"), ServiceRef("s")); f.Type = ""; return f },
			wantMsg: "type 非法",
		},
		{
			name: "ok reply 不得携带 error",
			rule: "reply 的 status 与 error 互斥：ok 就是没有结构化错误",
			frame: func() Frame {
				f := guardReply()
				f.Error = &Error{Code: CodeInternal, Message: "boom"}
				return f
			},
			wantMsg: "ok reply 不得带 error",
		},
		{
			name: "stream.seq 必须从 1 开始",
			rule: "流式块位置从 1 计数（seq<1 没有语义）",
			frame: func() Frame {
				f := guardStream()
				f.Stream.Seq = 0
				return f
			},
			wantMsg: "stream.seq 必须 >=1",
		},
		{
			name: "stream.seq 负数同样非法",
			rule: "流式块位置从 1 计数（seq<1 没有语义）",
			frame: func() Frame {
				f := guardStream()
				f.Stream.Seq = -3
				return f
			},
			wantMsg: "stream.seq 必须 >=1",
		},
		{
			name: "非终态 stream 不得带 status",
			rule: "只有终态块（end=true）才携带 status/error",
			frame: func() Frame {
				f := guardStream()
				f.Status = StatusOK
				return f
			},
			wantMsg: "非终态 stream 不得带 status/error",
		},
		{
			name: "非终态 stream 不得带 error",
			rule: "只有终态块（end=true）才携带 status/error",
			frame: func() Frame {
				f := guardStream()
				f.Error = &Error{Code: CodeInternal, Message: "boom"}
				return f
			},
			wantMsg: "非终态 stream 不得带 status/error",
		},
		{
			name: "stream.end 必须带 status",
			rule: "终态块必须自报结局（ok/error）",
			frame: func() Frame {
				f := guardStream()
				f.Stream.End = true
				return f
			},
			wantMsg: "stream.end 必须带 status",
		},
		{
			name: "error 终态 stream 必须带 error",
			rule: "status=error 的终态帧必须给出结构化错误",
			frame: func() Frame {
				f := guardStream()
				f.Stream.End = true
				f.Status = StatusError
				return f
			},
			wantMsg: "error stream.end 必须带 error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.frame()
			err := f.Validate()
			if err == nil {
				t.Fatalf("guard 失效：非法帧被接受（规则：%s）\n帧 = %+v", tc.rule, f)
			}
			if !IsCode(err, CodeInvalidArgument) {
				t.Fatalf("错误码 = %v, want invalid_argument（规则：%s）", err, tc.rule)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("错误信息 = %q, 应包含 %q（规则：%s）", err, tc.wantMsg, tc.rule)
			}
		})
	}
}

// TestValidateAuthorityMatrix 钉住「结果归属逐跳自定、不可丢失」的取值闭合：
// 每个 kind 只允许自己的 authority 集合，越界的必须被拒绝，合法的必须放行。
// 正向用例和反向用例放在一起，是为了防止「把 guard 改成永远报错」也算通过。
func TestValidateAuthorityMatrix(t *testing.T) {
	cases := []struct {
		name     string
		from     Ref
		wantCode string // 空串表示应当放行
		wantMsg  string
	}{
		{
			name:     "human 只能 owner/none：runtime 越界",
			from:     HumanRef("p").WithAuthority(AuthorityRuntime),
			wantCode: CodeInvalidArgument,
			wantMsg:  "human authority 只能是 owner/none",
		},
		{
			name:     "human 只能 owner/none：delegated 越界",
			from:     HumanRef("p").WithAuthority(AuthorityDelegated),
			wantCode: CodeInvalidArgument,
			wantMsg:  "human authority 只能是 owner/none",
		},
		{
			name:     "human 空 authority 非法（Normalize 之前）",
			from:     Ref{Kind: "human", ID: "p"},
			wantCode: CodeInvalidArgument,
			wantMsg:  "human authority 只能是 owner/none",
		},
		{
			name:     "agent 的 delegated 在 v2.1 未设计",
			from:     AgentRef("a").WithAuthority(AuthorityDelegated),
			wantCode: CodeNotSupported,
			wantMsg:  "delegated 在 v2.1 未设计",
		},
		{
			name:     "agent 未知 authority 必须拒绝",
			from:     AgentRef("a").WithAuthority("superuser"),
			wantCode: CodePermissionDenied,
			wantMsg:  "agent authority 只能是 none/runtime",
		},
		{
			name:     "agent 空 authority 非法（Normalize 之前）",
			from:     Ref{Kind: "agent", ID: "a"},
			wantCode: CodePermissionDenied,
			wantMsg:  "agent authority 只能是 none/runtime",
		},
		{
			name:     "service 只能 none：owner 越界",
			from:     ServiceRef("s").WithAuthority(AuthorityOwner),
			wantCode: CodePermissionDenied,
			wantMsg:  "service/system authority 必须是 none",
		},
		{
			name:     "service 只能 none：runtime 越界",
			from:     ServiceRef("s").WithAuthority(AuthorityRuntime),
			wantCode: CodePermissionDenied,
			wantMsg:  "service/system authority 必须是 none",
		},
		{
			name:     "system 只能 none：owner 越界",
			from:     SystemRef("sys").WithAuthority(AuthorityOwner),
			wantCode: CodePermissionDenied,
			wantMsg:  "service/system authority 必须是 none",
		},
		{
			name:     "system 只能 none：delegated 越界",
			from:     SystemRef("sys").WithAuthority(AuthorityDelegated),
			wantCode: CodePermissionDenied,
			wantMsg:  "service/system authority 必须是 none",
		},
		// 正向：合法组合不得被误伤。
		{name: "human + owner 合法", from: HumanRef("p")},
		{name: "human + none 合法", from: HumanRef("p").WithAuthority(AuthorityNone)},
		{name: "agent + none 合法", from: AgentRef("a")},
		{name: "agent + runtime 合法", from: AgentRef("a").WithAuthority(AuthorityRuntime)},
		{name: "service + none 合法", from: ServiceRef("s")},
		{name: "system + none 合法", from: SystemRef("sys")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := guardCall(tc.from, ServiceRef("s"))
			err := f.Validate()
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("合法组合被拒绝：from=%+v err=%v", tc.from, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("guard 失效：非法 authority 被接受，from=%+v", tc.from)
			}
			if !IsCode(err, tc.wantCode) {
				t.Fatalf("错误码 = %v, want %s（from=%+v）", err, tc.wantCode, tc.from)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("错误信息 = %q, 应包含 %q", err, tc.wantMsg)
			}
		})
	}
}

// TestAgentAuthorityErrorMessageAdvertisesOnlyLegalValues 钉住一条运维事实：
// 报错信息里列出的取值集合必须和代码允许的一致。
// 旧文案写「只能是 none/runtime/delegated」，但 delegated 在上一行就被 not_supported 拒了，
// 照着报错去发 delegated 的运维同学只会撞第二个错——这条用例保证报错不再说谎。
func TestAgentAuthorityErrorMessageAdvertisesOnlyLegalValues(t *testing.T) {
	for _, authority := range []string{"superuser", "", "owner"} {
		t.Run("agent authority="+authority, func(t *testing.T) {
			f := guardCall(AgentRef("a").WithAuthority(authority), ServiceRef("s"))
			err := f.Validate()
			if err == nil {
				t.Fatalf("agent authority=%q 应被拒绝", authority)
			}
			if !IsCode(err, CodePermissionDenied) {
				t.Fatalf("错误码 = %v, want permission_denied", err)
			}
			msg := err.Error()
			if strings.Contains(msg, AuthorityDelegated) {
				t.Fatalf("报错把 delegated 说成合法取值，但代码拒绝它: %q", msg)
			}
			for _, want := range []string{AuthorityNone, AuthorityRuntime} {
				if !strings.Contains(msg, want) {
					t.Fatalf("报错应列出合法取值 %s, got %q", want, msg)
				}
			}
		})
	}
}

// TestDelegatedIsNotSupportedForAgent 单独强调：audit 里「delegated 被允许」的说法与代码不符。
// 这里锁定它必须是 not_supported，而不是放行，也不是 permission_denied。
func TestDelegatedIsNotSupportedForAgent(t *testing.T) {
	f := guardCall(AgentRef("a").WithAuthority(AuthorityDelegated), ServiceRef("s"))
	err := f.Validate()
	if !IsCode(err, CodeNotSupported) {
		t.Fatalf("agent + delegated = %v, want not_supported", err)
	}
}
