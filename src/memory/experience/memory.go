package experience

// 长期记忆的写与召回：remember / forget 与语义召回（embedding 不可用时回落 bigram）。
//
// 向量是派生数据：写入时算一次、失败只记日志；召回时懒补齐。
// 语义召回不可用（服务没上线 / 失败 / 超时）时静默回落 bigram，不阻塞本轮。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/memory/redact"
	"github.com/NouSart/sump/protocol"
)

// remember 写一条长期记忆（内容重复时幂等跳过）；来源记调用方服务名。
// 新建条目顺带算一次向量（派生数据；失败仅记日志——召回时会懒补齐）。
func (s *Server) remember(ctx context.Context, body []byte, meta callMeta) (any, error) {
	var p protocol.RememberPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if s.longterm() == nil {
		return nil, errors.New("长期记忆未启用")
	}
	// 向量算的是"库里那行字"：Add 会在写入边界脱敏，这里先脱敏一次拿到同一份文本，
	// 免得当场算的对应原文、懒补齐算的对应掩码（Text 可重复作用）。
	if math.IsNaN(p.Salience) || p.Salience < 0 || p.Salience > 1 {
		return nil, errors.New("remember: salience 需在 0..1")
	}
	content := redact.Text(p.Content)
	id, created, err := s.longterm().AddWithSalience(p.Kind, content, p.Priority, p.Salience, p.ConversationID, meta.from)
	if err != nil {
		return nil, fmt.Errorf("remember: %w", err)
	}
	if created {
		vecs, model, err := s.embedTexts(ctx, meta.trace, []string{content}, false)
		if err != nil {
			s.logger.Printf("remember 向量化失败（召回时将懒补齐）: %v", err)
		} else if err := s.longterm().SetEmbedding(id, vecs[0], model); err != nil {
			s.logger.Printf("remember 向量落库失败（召回时将懒补齐）: %v", err)
		}
	}
	return protocol.RememberResult{ID: id, Created: created}, nil
}

// forget 软删一条长期记忆（不暴露给模型——不给 LLM 删记忆的按钮；手动 / 直接跳转调用）。
func (s *Server) forget(body []byte) (any, error) {
	var p protocol.ForgetPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if s.longterm() == nil {
		return nil, errors.New("长期记忆未启用")
	}
	if p.ID <= 0 {
		return nil, errors.New("forget: id 必填")
	}
	ok, err := s.longterm().Forget(p.ID)
	if err != nil {
		return nil, fmt.Errorf("forget: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("forget: 条目 %d 不存在或已删除", p.ID)
	}
	return protocol.ForgetResult{ID: p.ID}, nil
}

// rankRelevant 返回"相关记忆"条目：语义召回为主、bigram 兜底（embedding 缺失/失败/超时 → 静默回落）。
// now 参与生命周期加权；阈值仍只看原始相似度。
func (s *Server) rankRelevant(ctx context.Context, trace string, entries []longterm.Entry, query string, limit, maxChars int, now time.Time) []longterm.Entry {
	if strings.TrimSpace(query) == "" || len(entries) == 0 {
		return nil
	}
	if ranked, ok := s.semanticRank(ctx, trace, entries, query, limit, maxChars, now); ok {
		return ranked
	}
	return longterm.RankLifecycle(entries, query, limit, maxChars, now)
}

// semanticRank：查询向量 + 条目向量（缺失/换模型后懒补齐）→ 余弦排序。
// 返回 ok=false 表示"语义召回不可用"（调用方回落 bigram）；此时不得返回部分结果。
func (s *Server) semanticRank(ctx context.Context, trace string, entries []longterm.Entry, query string, limit, maxChars int, now time.Time) ([]longterm.Entry, bool) {
	qvecs, model, err := s.embedTexts(ctx, trace, []string{query}, true)
	if err != nil {
		s.logger.Printf("语义召回不可用（回落 bigram）: %v", err)
		return nil, false
	}
	if err := s.backfillEmbeddings(ctx, trace, entries, model); err != nil {
		s.logger.Printf("条目向量补齐部分失败（已有向量的条目照常参与）: %v", err)
	}
	return longterm.RankSemanticLifecycle(entries, qvecs[0], limit, maxChars, recallMinScore, now), true
}

// backfillEmbeddings 为缺失/失效（模型不一致）的条目补算向量并落库（就地更新 entries）。
// 派生数据原则：补齐失败不影响已有向量条目；失败仅记录（下次召回再试）。
func (s *Server) backfillEmbeddings(ctx context.Context, trace string, entries []longterm.Entry, model string) error {
	var missing []int
	for i := range entries {
		if len(entries[i].Embedding) == 0 || entries[i].EmbeddingModel != model {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	const chunk = 16
	var firstErr error
	for start := 0; start < len(missing); start += chunk {
		end := start + chunk
		if end > len(missing) {
			end = len(missing)
		}
		if end-start > s.maxBatchHint() {
			end = start + s.maxBatchHint()
		}
		idx := missing[start:end]
		texts := make([]string, 0, len(idx))
		for _, i := range idx {
			texts = append(texts, entries[i].Content)
		}
		vecs, vecModel, err := s.embedTexts(ctx, trace, texts, false)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		for j, i := range idx {
			entries[i].Embedding = vecs[j]
			entries[i].EmbeddingModel = vecModel
			if err := s.longterm().SetEmbedding(entries[i].ID, vecs[j], vecModel); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// maxBatchHint 每次 embed 的批大小（embedding 服务默认上限 32；这里取保守值）。
func (s *Server) maxBatchHint() int { return 16 }

// embedTexts 调用 embedding 服务：批量文本 → 向量（限时；失败即返回错误）。
// 线路上的向量是 float64（Python 原生）；这里转成 float32 供存储（派生数据的存储格式）。
func (s *Server) embedTexts(ctx context.Context, trace string, texts []string, query bool) ([][]float32, string, error) {
	name := s.embeddingName()
	if name == "" {
		return nil, "", errors.New("未配置 embedding 服务")
	}
	cctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	data, err := s.Call(cctx, name, "embed", protocol.EmbedPayload{Texts: texts, Query: query}, trace)
	if err != nil {
		return nil, "", err
	}
	var res protocol.EmbedResult
	if err := protocol.DecodeRaw(data, &res); err != nil {
		return nil, "", fmt.Errorf("embed 结果解析失败: %w", err)
	}
	if len(res.Vectors) != len(texts) {
		return nil, "", fmt.Errorf("embed 返回向量数不符: %d != %d", len(res.Vectors), len(texts))
	}
	vecs := make([][]float32, len(res.Vectors))
	for i, v := range res.Vectors {
		out := make([]float32, len(v))
		for j, f := range v {
			out[j] = float32(f)
		}
		vecs[i] = out
	}
	return vecs, res.Model, nil
}
