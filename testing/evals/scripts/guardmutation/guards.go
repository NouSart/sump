package main

// 异变清单：本轮整改立起来的每一道守卫，各配一条"看起来合理但语义错了"的改动。
//
// 为什么要单独一份而不是只留 evals 那套：evals 的清单守的是评测器自己的判定，
// 而这轮整改改的是**生产路径上的安全与正确性判断**（写入边界、SSRF 守卫、协议帧、
// 审批档位、送达判定）。这些判断的测试是本轮新写的，"测试写完了"不等于"测试守得住"——
// 只有把守卫本身改错、看测试是否翻脸，才知道红线是真有人看着还是写在注释里。
//
// 每条的选法都一样：挑那种**改完代码仍能编译、仍能跑、看上去还说得通**的写法。
// 随机改字符没意义，改出来的编译错误只会证明编译器在工作。
var guards = []guard{
	// ---------- 共享库写入边界（src/memory/redact）----------
	{
		name: "复检谓词认不出 UNC 路径，存量清理自称干净",
		file: "src/memory/redact/redact.go",
		old:  `	case reWinPath.MatchString(s), reUNCPath.MatchString(s), rePosix.MatchString(s):`,
		new:  `	case reWinPath.MatchString(s), rePosix.MatchString(s):`,
	},
	{
		name: "密钥形状不再算泄露（sk-… 原样进库）",
		file: "src/memory/redact/redact.go",
		old:  `	case reToken.MatchString(s), reBearer.MatchString(s):`,
		new:  `	case reBearer.MatchString(s):`,
	},
	{
		name: "会话键判定放宽：任何值都算已收口",
		file: "src/memory/redact/redact.go",
		old:  `	return reOpaqueSession.MatchString(s) || reOpaqueBare.MatchString(s) || s == ""`,
		new:  `	return true`,
	},
	{
		name: "摘要位数变短，产出的形状不再是自己认的那一种",
		file: "src/memory/redact/redact.go",
		old:  `	return hex.EncodeToString(sum[:])[:12]`,
		new:  `	return hex.EncodeToString(sum[:])[:8]`,
	},
	{
		name: "标识列不再收口：带冒号 / at 的值照样写进 task_id",
		file: "src/memory/redact/redact.go",
		old:  `	if !reRefID.MatchString(s) {`,
		new:  `	if false && !reRefID.MatchString(s) {`,
	},

	// ---------- 会话目录的两个标题列（src/memory/shared）----------
	// 用例先填实（d3174af：Publish 输入里两个标题原本都是空串，断言对着空格永真），再补守卫。
	{
		name: "任务标题绕过写入边界（目录里的标题原样入库）",
		file: "src/memory/shared/sessions.go",
		old:  `	title := Sanitize(in.TaskTitle, maxTitleChars)`,
		new:  `	title := in.TaskTitle`,
	},
	{
		name: "会话标题绕过写入边界（门户上那行会话名没人管）",
		file: "src/memory/shared/sessions.go",
		old:  `	sessionTitle := Sanitize(in.SessionTitle, maxTitleChars)`,
		new:  `	sessionTitle := in.SessionTitle`,
	},

	// ---------- 存量清理（src/memory/repair）----------
	{
		name: "复检没归零也照样提交（洗了一半写进库）",
		file: "src/memory/repair/repair.go",
		old:  `		if len(rep.Remaining) > 0 {`,
		new:  `		if false && len(rep.Remaining) > 0 {`,
	},
	{
		name: "跳过复检，报告里的「剩余」永远是空",
		file: "src/memory/repair/repair.go",
		old:  `	rep.Remaining, err = verifyFn(tx)`,
		new:  `	rep.Remaining, err = nil, nil`,
	},
	{
		name: "清理清单漏掉一列（那张表永远不被检）",
		file: "src/memory/repair/repair.go",
		old:  `		{"anchor", kText},`,
		new:  `		// anchor 是种子句，写死的，不用洗`,
	},

	// ---------- 经验网向量与出厂参数（src/memory/memnet）----------
	{
		name: "unit 回到就地改写（调用方手里的向量被偷偷归一化）",
		file: "src/memory/memnet/memnet.go",
		old: `	out := make([]float32, len(v))
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		copy(out, v)
		return out
	}
	n = math.Sqrt(n)
	for i := range v {
		out[i] = float32(float64(v[i]) / n)
	}
	return out`,
		new: `	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		return v
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v`,
	},
	{
		name: "加挂阈值高过中心化后的带宽（「网」悄悄退化成单父树）",
		file: "src/memory/memnet/memnet.go",
		old:  `		LinkL1:      0.15,`,
		new:  `		LinkL1:      0.25,`,
	},

	// ---------- 出站下载守卫（src/protocol/fetchsafe）----------
	{
		name: "协议白名单放开：file:// 也算 http",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old:  `	if u.Scheme != "http" && u.Scheme != "https" {`,
		new:  `	if u.Scheme == "" {`,
	},
	{
		name: "链路本地不再拒（云元数据地址 169.254.169.254 放行）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old: `	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "链路本地地址"`,
		new: `	case ip.IsLinkLocalMulticast():
		return "组播地址"`,
	},
	{
		name: "重定向跳按逃生闸口径检查（借中转页回到本机）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old:  `			if r := p.check(req.URL.String(), false); r != nil {`,
		new:  `			if r := p.check(req.URL.String(), p.AllowLoopback); r != nil {`,
	},
	{
		name: "超限探测的余量被省掉（超长内容被静默截断成「合法」）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old:  `	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))`,
		new:  `	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))`,
	},
	{
		name: "拨号前不再复查（校验与连接之间换答案）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old: `		if ip := net.ParseIP(host); ip != nil {
			if why := dangerOf(ip, p.AllowLoopback); why != "" {`,
		new: `		if ip := net.ParseIP(host); ip != nil {
			if why := dangerOf(ip, true); why != "" {`,
	},

	// ---------- 协议参考实现（src/spv2）----------
	{
		name: "reply_to 的 msgpack 键名漂成驼峰（对端读不到关联）",
		file: "src/spv2/frame.go",
		old:  `	ReplyTo        string  ` + "`" + `msgpack:"reply_to,omitempty" json:"reply_to,omitempty"` + "`",
		new:  `	ReplyTo        string  ` + "`" + `msgpack:"replyTo,omitempty" json:"replyTo,omitempty"` + "`",
	},
	{
		name: "未知 method 回了错误但码不是 not_found",
		file: "src/spv2/server.go",
		old:  `			reply := NewErrorReply(f, s.card.Actor, CodeNotFound, "unknown method: "+f.Method, false, nil)`,
		new:  `			reply := NewErrorReply(f, s.card.Actor, CodeInternal, "unknown method: "+f.Method, false, nil)`,
	},
	{
		name: "幂等缓存对所有 method 生效（有副作用的 method 被重放）",
		file: "src/spv2/server.go",
		old:  `		if m, ok := s.card.Method(f.Method); ok && m.Idempotent {`,
		new:  `		if _, ok := s.card.Method(f.Method); ok {`,
	},

	// ---------- 隔离与记账（src/spv2 · 由 P3 补上）----------
	{
		name: "panic 被静默吞掉：不记账也不说是怎么回事",
		file: "src/spv2/server.go",
		old:  `	s.metrics.Counter("sump_panics_total", "Recovered handler panics", map[string]string{"method": s.methodLabel(f.Method)}, 1)`,
		new:  `	// 只隔离，不记账`,
	},
	{
		name: "校验通过的 reply/stream 帧又被静默丢弃",
		file: "src/spv2/server.go",
		old:  `		s.countDropped(f, dropNoDispatcher, "")`,
		new:  `		_ = f`,
	},
	{
		name: "帧丢了只进指标不进日志（聚合看得见，单条查不着是谁丢的）",
		file: "src/spv2/server.go",
		old:  `	s.logger.Printf("%s", msg)`,
		new:  `	_ = msg`,
	},
	{
		name: "panic 的回执没算回执：event 那一半被记两次",
		file: "src/spv2/server.go",
		old: `			panicked = true
			s.countPanic(call.Frame, r)`,
		new: `			s.countPanic(call.Frame, r)`,
	},
	{
		name: "校验失败的 call 不再补错误回执（账记了，调用方却还在空等）",
		file: "src/spv2/server.go",
		old:  `	if f.Type == TypeCall {`,
		new:  `	if false {`,
	},

	// ---------- 流式有界（src/spv2 · spec §3.5/§10）----------
	{
		name: "同时打开的流不再受 max_stream 限制",
		file: "src/spv2/server.go",
		old:  "		if maxStreams > 0 && len(c.streams) >= maxStreams {",
		new:  "		if false && maxStreams > 0 && len(c.streams) >= maxStreams {",
	},
	{
		name: "单流字节不再受 max_stream_buffer 限制",
		file: "src/spv2/server.go",
		old:  "	if maxBuffer > 0 && st.bytes > maxBuffer {",
		new:  "	if false && maxBuffer > 0 && st.bytes > maxBuffer {",
	},
	{
		name: "发送侧 stream.seq 不再要求逐 1 递增",
		file: "src/spv2/server.go",
		old:  "	} else if seq != st.lastSeq+1 {",
		new:  "	} else if false {",
	},
	{
		name: "接收侧 seq 缺口/重复不再记账",
		file: "src/spv2/client.go",
		old:  `	if kind != "" && c.metrics != nil {`,
		new:  `	if false && kind != "" && c.metrics != nil {`,
	},

	// ---------- 指标契约（src/spv2 · spec §12.1/§12.2）----------
	{
		name: "label 白名单失效（work/actor/tenant 进指标）",
		file: "src/spv2/metrics.go",
		old:  `	labels = sanitizeLabels(labels)`,
		new:  `	_ = labels`,
	},
	{
		name: "未知 method 原名带进指标（基数可被打爆）",
		file: "src/spv2/server.go",
		old: `	if _, ok := s.handlers[method]; ok {
		return method
	}
	return "other"`,
		new: `	return method`,
	},
	{
		name: "OpenMetrics 不再写 EOF",
		file: "src/spv2/http.go",
		old:  `	b.WriteString("# EOF\n")`,
		new:  `	_ = b`,
	},
	{
		name: "直方图不再输出 +Inf 桶",
		file: "src/spv2/http.go",
		old:  `			fmt.Fprintf(&b, "%s_bucket%s %d\n", m.Name, labelsWith(m.Labels, "le", "+Inf"), m.Count)` + "\n",
		new:  "",
	},
	{
		name: "/metrics 退回 Prometheus 0.0.4 文本",
		file: "src/spv2/http.go",
		old:  `	w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")`,
		new:  `	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")`,
	},
	// ---------- 错误语义（src/spv2 · spec §8/§6.6）----------
	{
		name: "deadline_exceeded 不再 retryable（§8 表被改）",
		file: "src/spv2/errors.go",
		old:  `	case CodeDeadlineExceeded, CodeResourceExhausted, CodeUnavailable, CodeInternal:`,
		new:  `	case CodeResourceExhausted, CodeUnavailable, CodeInternal:`,
	},
	{
		name: "ctx 取消被塌缩成 internal",
		file: "src/spv2/server.go",
		old: `	case errors.Is(err, context.Canceled):
		return CodeCancelled
	`,
		new: "",
	},
	{
		name: "非 *Error 的 retryable 默认 false",
		file: "src/spv2/server.go",
		old:  `	return RetryableByCode(codeOf(err))`,
		new:  `	return false`,
	},
	{
		name: "拨号失败回裸网络错误（不是 unavailable）",
		file: "src/spv2/client.go",
		old:  `		return nil, ErrCode(CodeUnavailable, "连接 "+url+" 失败: "+err.Error(), true, nil)`,
		new:  `		return nil, err`,
	},
	{
		name: "MemStore 对 stalled work 继续扣租约",
		file: "src/spv2/store.go",
		old: `	if rec.work.Status == WorkStalled {
		return rec.remaining, ErrCode(CodeStalled, "work "+workID+" 已 stalled（先 renew/resume）", false, nil)
	}
	`,
		new: "",
	},
	{
		name: "SQLiteStore 对 stalled work 继续扣租约",
		file: "src/spv2/sqlitestore.go",
		old: `	if status == WorkStalled {
		return rem, ErrCode(CodeStalled, "work "+workID+" 已 stalled（先 renew/resume）", false, nil)
	}
	`,
		new: `	_ = status
`,
	},
	// ---------- 参考实现最小指标（src/spv2/ref · spec §12.3）----------
	{
		name: "context 建 work 不记 work_active",
		file: "src/spv2/ref/memory.go",
		old: `	m.trackWork(work)
	return call.Reply(ContextResponse{Messages: req.Messages, Work: work})`,
		new: `	return call.Reply(ContextResponse{Messages: req.Messages, Work: work})`,
	},
	{
		name: "message_post 不记 a2a pending",
		file: "src/spv2/ref/memory.go",
		old: `	m.trackA2A(spv2.MessagePending)
	return call.Reply(MessagePostResponse{ID: id})`,
		new: `	return call.Reply(MessagePostResponse{ID: id})`,
	},
	{
		name: "message_claim 不记 a2a claimed",
		file: "src/spv2/ref/memory.go",
		old: `	if msg != nil {
		m.trackA2A(spv2.MessageClaimed)
	}`,
		new: `	_ = msg`,
	},
	{
		name: "message_ack 不记 a2a delivered",
		file: "src/spv2/ref/memory.go",
		old: `	m.trackA2A(spv2.MessageDelivered)
	return call.Reply(MessageAckResponse{Acked: true})`,
		new: `	return call.Reply(MessageAckResponse{Acked: true})`,
	},
	{
		name: "lease 耗尽不记 lease_exhausted_total",
		file: "src/spv2/ref/memory.go",
		old: `		if errors.Is(err, spv2.ErrLeaseExhausted) {
			m.metrics().Counter("sump_lease_exhausted_total", "Lease exhausted", nil, 1)
		}
	`,
		new: "",
	},
	{
		name: "state label 被指标白名单丢掉",
		file: "src/spv2/metrics.go",
		old:  `"state": true, "kind": true,`,
		new:  `"kind": true,`,
	},
	// ---------- LLM / 工具域指标（src/reasoner · spec §12.3）----------
	{
		name: "LLM 请求计数不累加",
		file: "src/reasoner/server/metrics.go",
		old:  `	reg.Counter("sump_llm_requests_total", "LLM requests", map[string]string{"model": model, "outcome": outcome}, 1)`,
		new:  `	reg.Counter("sump_llm_requests_total", "LLM requests", map[string]string{"model": model, "outcome": outcome}, 0)`,
	},
	{
		name: "prompt token 不计数",
		file: "src/reasoner/server/metrics.go",
		old:  `		reg.Counter("sump_llm_tokens_total", "LLM tokens", map[string]string{"model": model, "kind": "prompt"}, float64(usage.PromptTokens))` + "\n",
		new:  "",
	},
	{
		name: "LLM 延迟不入直方图",
		file: "src/reasoner/server/metrics.go",
		old:  `	reg.Observe("sump_llm_latency_seconds", "LLM latency", "s", map[string]string{"model": model}, elapsed.Seconds())` + "\n",
		new:  "",
	},
	{
		name: "工具错误不计数（只记成功）",
		file: "src/reasoner/server/metrics.go",
		old:  `	reg.Counter("sump_tool_calls_total", "Tool calls", map[string]string{"tool": tool, "outcome": outcome}, 1)`,
		new: `	if err == nil {
		reg.Counter("sump_tool_calls_total", "Tool calls", map[string]string{"tool": tool, "outcome": outcome}, 1)
	}`,
	},
	{
		name: "Fake 不回报 usage",
		file: "src/reasoner/llm/fake.go",
		old:  `	return ChatResult{Message: f.Replies[idx], Usage: f.Usage}, nil`,
		new:  `	return ChatResult{Message: f.Replies[idx]}, nil`,
	},
	{
		name: "tool label 被指标白名单丢掉",
		file: "src/spv2/metrics.go",
		old:  `"state": true, "kind": true, "tool": true,`,
		new:  `"state": true, "kind": true,`,
	},
	// ---------- §5.1 hello 握手（src/spv2）----------
	{
		name: "服务端不再要求首帧 hello",
		file: "src/spv2/server.go",
		old:  `	if !c.helloOK() && !(f.Type == TypeCall && f.Method == "hello") {`,
		new:  `	if false {`,
	},
	{
		name: "客户端 Call 不先握手",
		file: "src/spv2/client.go",
		old: `	if err := c.ensureHandshake(f.From, f.To); err != nil {
		return Frame{}, err
	}
	return c.callRaw(ctx, f, f.ID)`,
		new: `	return c.callRaw(ctx, f, f.ID)`,
	},
	{
		name: "hello 成功不标记握手完成",
		file: "src/spv2/server.go",
		old: `	if c.conn != nil {
		c.conn.markHello()
	}
	`,
		new: "",
	},
	{
		name: "客户端 Send 不先握手",
		file: "src/spv2/client.go",
		old: `func (c *Client) Send(f Frame) error {
	if err := c.ensureHandshake(f.From, f.To); err != nil {
		return err
	}
	return c.send(f)
}`,
		new: `func (c *Client) Send(f Frame) error {
	return c.send(f)
}`,
	},
	// ---------- token 租约与 done 前置条件（src/reasoner · spec §6.4/§17.2）----------
	{
		name: "token 租约不走真实扣减",
		file: "src/reasoner/server/lease.go",
		old: `	if s.tokenChargeHook != nil {
		return s.tokenChargeHook(ctx, trace, tokens)
	}
`,
		new: "",
	},
	{
		name: "有在飞工具仍标 done",
		file: "src/reasoner/server/board.go",
		old: `	if pending > 0 {
		s.logger.Printf("会话 %s：还有 %d 个在飞工具，work 记 waiting（不能 done）", st.cid, pending)
		s.updateWorkStatus(st.trace, "waiting")
	} else {
		s.updateWorkStatus(st.trace, "done")
	}
`,
		new: `	_ = pending
	s.updateWorkStatus(st.trace, "done")
`,
	},
	// ---------- message 入口（src/reasoner · spec §7.1）----------
	{
		name: "service 入口不要求 sender",
		file: "src/reasoner/server/message.go",
		old:  `		if p.Sender == nil {`,
		new:  `		if false && p.Sender == nil {`,
	},
	{
		name: "service 入口假受理不进人类主链",
		file: "src/reasoner/server/message.go",
		old: `		if p.Sender == nil {
			return nil, &permissionError{msg: fmtMessageDenied(from, "service 必须带 sender（入口不能冒充人类）")}
		}
		return s.humanMessage(ctx, from.ID, trace, p)`,
		new: `		if p.Sender == nil {
			return nil, &permissionError{msg: fmtMessageDenied(from, "service 必须带 sender（入口不能冒充人类）")}
		}
		return map[string]any{"status": "accepted"}, nil`,
	},
	// ---------- 流式 message（src/reasoner · spec §16.3）----------
	{
		name: "token 增量不再回调给调用方",
		file: "src/reasoner/llm/client.go",
		old:  `			onDelta(choice.Delta.Content)`,
		new:  `			_ = choice.Delta.Content`,
	},

	{
		name: "流式终态不带最终答案",
		file: "src/reasoner/server/stream.go",
		old:  `		return msgs[i].Content`,
		new:  `		return ""`,
	},
	// ---------- A2A 最小闭环（src/reasoner · spec §6.8）----------
	{
		name: "agent 带目标 work 的消息不入队",
		file: "src/reasoner/server/message.go",
		old:  `	if p.Work != "" && p.Work != trace {`,
		new:  `	if false && p.Work != "" && p.Work != trace {`,
	},
	{
		name: "开任务时不注入 A2A 队列",
		file: "src/reasoner/server/infer.go",
		old:  `	} else if len(queued) > 0 {`,
		new:  `	} else if false && len(queued) > 0 {`,
	},
	// ---------- 设置键改名（src/memory · recall.* → context.*）----------
	{
		name: "设置键偷偷退回 recall.*（别名兼容）",
		file: "src/memory/server/server.go",
		old:  `	s.HandleSetting("context.history_limit"`,
		new:  `	s.HandleSetting("recall.history_limit"`,
	},
	// ---------- work 推进指标（src/memory · spec §12.3）----------
	{
		name: "work 推进指标用未白名单 label（被静默丢掉）",
		file: "src/memory/server/a2a.go",
		old:  `map[string]string{"status": to}`,
		new:  `map[string]string{"to": to}`,
	},
	// ---------- 未设计 feature 的 not_supported（spec §17.3/§18）----------
	{
		name: "tenant 被静默忽略",
		file: "src/spv2/validate.go",
		old: `	if f.From.Tenant != "" || f.To.Tenant != "" {
		return ErrCode(CodeNotSupported, "tenant 在 v2.1 未设计", false, nil)
	}
`,
		new: "",
	},
	{
		name: "spawn/drain/retire 退化成 not_found",
		file: "src/spv2/server.go",
		old: `		if notDesignedMethods[f.Method] {
			reply := NewErrorReply(f, s.card.Actor, CodeNotSupported, f.Method+" 在 v2.1 未设计", false, nil)
			s.countError(CodeNotSupported)
			s.observeResponseBytes(f.Method, len(reply.Body))
			_ = c.send(reply)
			return
		}
`,
		new: "",
	},
	// ---------- qq entry deliver 归属（src/qq · spec §6.5）----------
	{
		name: "qq deliver 放行任意 agent",
		file: "src/qq/server/server.go",
		old: `	if from.Kind == "agent" && from.Authority == spv2.AuthorityRuntime && from.ID == work.Agent.ID {
		return true
	}`,
		new: `	if from.Kind == "agent" {
		return true
	}`,
	},
	// ---------- deliver 归属（src/reasoner · spec §6.5）----------
	{
		name: "deliver 不带 runtime authority",
		file: "src/reasoner/server/server.go",
		old:  `	return spv2.AgentRef(s.cfg.Name).WithAuthority(spv2.AuthorityRuntime)`,
		new:  `	return spv2.AgentRef(s.cfg.Name)`,
	},
	// ---------- 流取消（src/spv2 · spec §10）----------
	{
		name: "transport cancel 不释放流账本",
		file: "src/spv2/server.go",
		old:  `	n := c.cancelStream(req.Stream)`,
		new: `	n := 0
	_ = c.cancelStream`,
	},
	{
		name: "带 stream 的 cancel 不拦截",
		file: "src/spv2/server.go",
		old:  `	if req.Stream != "" {`,
		new:  `	if false && req.Stream != "" {`,
	},
	{
		name: "客户端取消后不关本地流通道",
		file: "src/spv2/client.go",
		old: `	if ch := c.streams[callID]; ch != nil {
			delete(c.streams, callID)
			close(ch)
		}`,
		new: `	if ch := c.streams[callID]; ch != nil {
			delete(c.streams, callID)
			_ = ch
		}`,
	},
	// ---------- A2A 队列指标（src/memory · spec §12.3）----------
	{
		name: "A2A 队列深度不再写进注册表",
		file: "src/memory/server/a2a.go",
		old:  `	for _, state := range []string{store.WorkMsgPending, store.WorkMsgClaimed, store.WorkMsgDelivered, store.WorkMsgFailed} {`,
		new:  `	for _, state := range []string{store.WorkMsgDelivered} {`,
	},
	// ---------- message 权限矩阵（src/reasoner · spec §6.5/§7.1）----------
	{
		name: "human 不带 owner 也放行",
		file: "src/reasoner/server/message.go",
		old:  `	if from.Authority != spv2.AuthorityOwner {`,
		new:  `	if false && from.Authority != spv2.AuthorityOwner {`,
	},
	{
		name: "agent 带 runtime 也放行",
		file: "src/reasoner/server/message.go",
		old: `	if from.Authority != spv2.AuthorityNone {
			return nil, &permissionError{msg: fmtMessageDenied(from, "agent 的 message 永远 authority=none")}
`,
		new: `	if false && from.Authority != spv2.AuthorityNone {
			return nil, &permissionError{msg: fmtMessageDenied(from, "agent 的 message 永远 authority=none")}
`,
	},
	{
		name: "agent 没有任务也注入",
		file: "src/reasoner/server/message.go",
		old:  `	if st == nil {`,
		new:  `	if false && st == nil {`,
	},
	// ---------- max_header 与审批指标（spec §3.5/§12.3）----------
	{
		name: "不检查 max_header",
		file: "src/spv2/server.go",
		old: `	// spec §3.5 max_header：帧头（不含 body）超限同样拒绝，不能等解完大 body 才说。
	if max := s.opts.MaxHeader; max > 0 {
		headerOnly := f
		headerOnly.Body = nil
		if raw, err := Marshal(headerOnly); err == nil && int64(len(raw)) > max {
			s.reject(c, f, dropMaxHeader, CodeResourceExhausted, "header 超过 max_header", false, nil)
			return
		}
	}
`,
		new: "",
	},
	{
		name: "max_header 默认值丢失",
		file: "src/spv2/server.go",
		old:  `			o.MaxHeader = 16 << 10`,
		new:  `			o.MaxHeader = 0`,
	},
	{
		name: "审批决策不计数",
		file: "src/approval/server/review.go",
		old: `		s.recordApprovalDecision(protocol.DecisionAllow)
`,
		new: "",
	},
	{
		name: "decision label 被白名单丢掉",
		file: "src/spv2/metrics.go",
		old: `	"decision": true,
`,
		new: "",
	},
	// ---------- A2A 队列与 work 生命周期（src/memory · spec §6.8/§7.4/§7.5）----------
	{
		name: "claim 不看目标 actor",
		file: "src/memory/store/a2a.go",
		old:  `AND (wm.to_id = "" OR wm.to_id = ?)`,
		new:  `AND (wm.to_id = "" OR ? != "")`,
	},
	{
		name: "nack 不设重试上限（永不 dead-letter）",
		file: "src/memory/store/a2a.go",
		old:  `	if m.Attempts >= maxAttempts {`,
		new:  `	if false && m.Attempts >= maxAttempts {`,
	},
	{
		name: "终态 work 仍可入队",
		file: "src/memory/store/a2a.go",
		old: `	if spv2.IsTerminal(w.Status) {
		return WorkMessage{}, spv2.ErrCode(spv2.CodeConflict, "work "+workID+" 已终态，不能再入队消息", false, nil)
	}`,
		new: `	if false && spv2.IsTerminal(w.Status) {
		return WorkMessage{}, spv2.ErrCode(spv2.CodeConflict, "work "+workID+" 已终态，不能再入队消息", false, nil)
	}`,
	},
	{
		name: "renew 允许把 deadline 改到过去",
		file: "src/memory/store/a2a.go",
		old:  `		if t.Before(time.Now()) {`,
		new:  `		if false && t.Before(time.Now()) {`,
	},
	// ---------- work sweep（src/memory · spec §6.9）----------
	{
		name: "sweep 不看 deadline",
		file: "src/memory/store/work.go",
		old:  `	WHERE status IN (?, ?, ?) AND deadline_at != "" AND deadline_at < ?`,
		new:  `	WHERE status IN (?, ?, ?) AND deadline_at != "" AND deadline_at < "9999-12-31T23:59:59Z"`,
	},
	{
		name: "sweep 不看终态",
		file: "src/memory/store/work.go",
		old:  `	WHERE status IN (?, ?, ?) AND deadline_at != "" AND deadline_at < ?`,
		new:  `	WHERE status != "" AND deadline_at != "" AND deadline_at < ?`,
	},
	// ---------- reasoner 租约接线（spec §6.4/§6.3）----------
	{
		name: "执行工具前不扣租约",
		file: "src/reasoner/server/lease.go",
		old: `	if err := s.chargeToolLease(ctx, trace); err != nil {
		return err
	}
`,
		new: "",
	},
	{
		name: "扣租约失败仍放行工具",
		file: "src/reasoner/server/lease.go",
		old: `	if err := s.chargeToolLease(ctx, trace); err != nil {
		return err
	}`,
		new: `	_ = s.chargeToolLease(ctx, trace)`,
	},
	{
		name: "交付后不推进 work done",
		file: "src/reasoner/server/board.go",
		old:  `	s.updateWorkStatus(st.trace, "done")`,
		new:  `	_ = st.trace`,
	},
	// ---------- 生产 work 账本（src/memory · spec §6/§6.8）----------
	{
		name: "context 不再登记 work 行",
		file: "src/memory/server/server.go",
		old: `	if err := s.ensureContextWork(ctx, meta, tc, task.Name); err != nil {
		return protocol.Task{}, nil, err
	}
`,
		new: "",
	},
	{
		name: "work 非法状态转移不再拦",
		file: "src/memory/store/work.go",
		old:  `	if !spv2.CanTransition(w.Status, status) {`,
		new:  `	if false && !spv2.CanTransition(w.Status, status) {`,
	},
	{
		name: "work 控制权不再校验",
		file: "src/memory/server/work.go",
		old: `	if from.ID != "" && (from.ID == w.Agent.ID || from.ID == w.Owner.ID || from.ID == w.Entry.ID) {
		return nil
	}
	return spv2.ErrCode(spv2.CodePermissionDenied, "无权控制 work "+w.ID, false, nil)`,
		new: `	_ = w
	return nil`,
	},
	{
		name: "stalled work 仍可扣租约",
		file: "src/memory/store/work.go",
		old:  `	if w.Status == spv2.WorkStalled {`,
		new:  `	if w.ID == "__never_stalled__" {`,
	},
	{
		name: "Release 不再 clamp 到初始上限",
		file: "src/memory/store/work.go",
		old: `		tools_left = MIN(tools_left + ?, max_tools),
		tokens_left = MIN(tokens_left + ?, max_tokens),
		approvals_left = MIN(approvals_left + ?, max_approvals),
		children_left = MIN(children_left + ?, max_children),
		depth_left = MIN(depth_left + ?, max_depth) WHERE id = ?`,
		new: `		tools_left = tools_left + ?, tokens_left = tokens_left + ?, approvals_left = approvals_left + ?,
		children_left = children_left + ?, depth_left = depth_left + ? WHERE id = ?`,
	},
	// ---------- 会话库两张脸（src/memory/store）----------
	{
		name: "归档的会话又冒回默认列表",
		file: "src/memory/store/conversations.go",
		old:  `	if !includeArchived {`,
		new:  `	if false && !includeArchived {`,
	},

	// ---------- 复盘归类（src/reasoner/server）----------
	{
		name: "偏好被降回遗忘曲线（「不该忘」变成会忘）",
		file: "src/reasoner/server/reflect.go",
		old:  `			dur = "core"`,
		new:  `			dur = "durable"`,
	},

	// ---------- 审批档位（src/approval/engine）----------
	{
		name: "中档黑名单漏掉一条系统级动作（档位再次倒挂）",
		file: "src/approval/engine/tier_medium.go",
		old:  `	"pkexec": "提权执行",`,
		new:  `	// pkexec 少见，先不列`,
	},

	// ---------- 送达判定（src/qq/napcat）----------
	{
		name: "没有 status 也没有 retcode 时当成送达",
		file: "src/qq/napcat/napcat.go",
		old:  `	if !hasStatus && !hasRetcode {`,
		new:  `	if false && !hasStatus && !hasRetcode {`,
	},

	// ---------- 缩略图编码（src/images/server）----------
	{
		name: "PNG 字节贴上 JPEG 标签（指鹿为马回到老路）",
		file: "src/images/server/window.go",
		old:  `	return buf.Bytes(), "image/png", nil`,
		new:  `	return buf.Bytes(), "image/jpeg", nil`,
	},

	// ---------- 审计闸门与已裁定白名单（src/memory/audit 与 cmd/sump-repair）----------
	// 七条各盯一处"改完照样编译、照样跑、看上去还说得通"的走法。
	// 隔离靠 cmd/sump-repair 与 audit 里那些单类场景：被删的检查必须是当时唯一能给出
	// 那个退出码的原因，别的类别全为空——所以删掉任何一条，只有它自己的用例翻脸。
	{
		name: "未裁定的命中不再破闸（novel 只打印不拦）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if len(novel) > 0 {`,
		new:  `	if false && len(novel) > 0 {`,
	},
	{
		name: "白名单过期不再破闸（清单与库脱钩当成功）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if len(stale) > 0 {`,
		new:  `	if false && len(stale) > 0 {`,
	},
	{
		name: "位置冲突不再破闸（键撞上而格子搬走也退出 0）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if len(conflicted) > 0 {`,
		new:  `	if false && len(conflicted) > 0 {`,
	},
	{
		name: "两个白名单开关不再互斥（谁赢由实现顺序决定）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if *noAdjudicated && *adjudicatedPath != "" {`,
		new:  `	if false && *noAdjudicated && *adjudicatedPath != "" {`,
	},
	{
		name: "已裁定的位置不对账（列与探针被改也照样放行）",
		file: "src/memory/audit/adjudicated.go",
		old: `		if entry.Table == f.Table && entry.RowKey == f.RowKey &&
			entry.Column == f.Column && entry.Probe == f.Probe {`,
		new: `		if entry.Table == f.Table && entry.RowKey == f.RowKey {`,
	},
	{
		name: "行键列写死 rowid（清单给了别的键列也不认）",
		file: "src/memory/audit/adjudicated.go",
		old:  `		keyColumn, ok := keyColumnOf(cols, a.Table, a.Column)`,
		new:  `		keyColumn, ok := "rowid", true`,
	},
	{
		name: "已知凭据按出现次数计（同一值在清单里写两遍被数两次）",
		file: "src/memory/audit/audit.go",
		old:  `		if seen[k] || !strings.Contains(text, k) {`,
		new:  `		if !strings.Contains(text, k) {`,
	},
}
