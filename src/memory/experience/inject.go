package experience

// 经验侧记忆注入：只返回四节文本，最近对话由门面补。
// viewer 预留给读取侧可见性过滤（先按旧行为占位）；origin 用于排除当前会话自己。

import (
	"context"
	"fmt"

	"github.com/NouSart/sump/memory/experience/compose"
	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) nativeInject(ctx context.Context, call *spv2.Call) error {
	var p protocol.InjectPayload
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "inject body 无效: "+err.Error(), false, nil)
	}
	out, err := s.injectSections(ctx, call.Frame.Work, p)
	if err != nil {
		return call.ReplyError(spv2.CodeInternal, "inject: "+err.Error(), true, nil)
	}
	return call.Reply(out)
}

// injectSections 组装经验四节：核心 / 相关 / 线头 / 其他会话。
// 只有组装成功才记访问（Touch）；查询为空时不召回相关记忆，与旧行为一致。
func (s *Server) injectSections(ctx context.Context, trace string, p protocol.InjectPayload) (protocol.InjectResult, error) {
	coreLimit, coreMaxChars, relLimit, relMaxChars := s.limits()
	recallNow := nowFunc()
	var out protocol.InjectResult
	var touched []int64
	if s.longterm() != nil {
		entries, err := s.longterm().ListActive()
		if err != nil {
			return protocol.InjectResult{}, fmt.Errorf("查询长期记忆失败: %w", err)
		}
		coreEntries := longterm.Core(entries, coreLimit, coreMaxChars)
		coreIDs := make(map[int64]bool, len(coreEntries))
		for _, e := range coreEntries {
			coreIDs[e.ID] = true
			touched = append(touched, e.ID)
			out.Core = append(out.Core, compose.EntryLine(e.Kind, e.Content))
		}
		rest := make([]longterm.Entry, 0, len(entries))
		for _, e := range entries {
			if !coreIDs[e.ID] {
				rest = append(rest, e)
			}
		}
		for _, e := range s.rankRelevant(ctx, trace, rest, p.Query, relLimit, relMaxChars, recallNow) {
			touched = append(touched, e.ID)
			out.Relevant = append(out.Relevant, compose.EntryLine(e.Kind, e.Content))
		}
	}
	out.Experience = s.experienceLines(ctx, trace, p.Query)
	out.Others = s.otherSessionLines(p.Origin)
	if len(touched) > 0 {
		if err := s.longterm().Touch(touched, recallNow); err != nil {
			s.logger.Printf("长期记忆访问计数失败（不影响本轮）: %v", err)
		}
	}
	return out, nil
}
