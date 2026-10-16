package service

// 秘密项（HandleSecret）的本地持久化。
//
// 为什么由服务自己存：秘密只该落在**用它的服务**手里。设置中心只把面板那一次提交转发
// 过来，既不落盘也不回显，所以服务重启后必须能自己把值带起来——这就是本文件存在的全部
// 理由（设置中心那边没有值可下发）。
//
// 落盘是**明文**（本机单用户场景，等同 .env）；路径由 Config.SecretFile 决定，
// 传 "-" 表示只放内存不落盘（一次性环境用）。文件格式：
//
//	{"version": 1, "secrets": {"llm.key": "sk-..."}}

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// secretFile 是秘密文件的形状（带版本号，便于以后换写法）。
type secretFile struct {
	Version int               `json:"version"`
	Secrets map[string]string `json:"secrets"`
}

// loadSecrets 在 Start 时把本地秘密读回来并逐项应用。
// 文件里**所有**键都进内存表（不在本服务声明里的也留着），这样落盘时不会把别人的键抹掉。
func (s *Service) loadSecrets() {
	if s.cfg.SecretFile == "-" {
		return
	}
	data, err := os.ReadFile(s.cfg.SecretFile)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Printf("秘密文件读取失败（当作没有）: %v", err)
		}
		return
	}
	var f secretFile
	if err := json.Unmarshal(data, &f); err != nil {
		// 解析失败不覆盖原文件：宁可这次没加载上，也别把人家的文件写坏。
		s.logger.Printf("秘密文件解析失败（本次不加载，文件保持原样）: %v", err)
		return
	}
	if len(f.Secrets) == 0 {
		return
	}

	s.settingsMu.Lock()
	kinds := make(map[string]settingKind, len(s.settings))
	for _, it := range s.settings {
		kinds[it.key] = it.kind
	}
	for key, value := range f.Secrets {
		s.secrets[key] = value
	}
	s.settingsMu.Unlock()

	for key, value := range f.Secrets {
		if kinds[key] != settingSecret {
			s.logger.Printf("秘密文件里的 %s 不是本服务声明的秘密项（保留在文件里，不应用）", key)
			continue
		}
		if err := s.applySetting(key, value); err != nil {
			s.logger.Printf("秘密 %s 加载失败: %v", key, err)
			continue
		}
		s.logger.Printf("秘密 %s 已从本地文件加载", key)
	}
}

// saveSecret 记下一个秘密项并落盘；空值 = 清除该项（reset_setting 走这条）。
func (s *Service) saveSecret(key, value string) error {
	s.settingsMu.Lock()
	if value == "" {
		delete(s.secrets, key)
	} else {
		s.secrets[key] = value
	}
	snapshot := make(map[string]string, len(s.secrets))
	for k, v := range s.secrets {
		snapshot[k] = v
	}
	s.settingsMu.Unlock()
	return s.writeSecrets(snapshot)
}

// writeSecrets 原子落盘（先写临时文件再改名），避免半个文件。
func (s *Service) writeSecrets(secrets map[string]string) error {
	if s.cfg.SecretFile == "-" {
		return nil
	}
	if dir := filepath.Dir(s.cfg.SecretFile); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("秘密目录不可用: %w", err)
		}
	}
	data, err := json.MarshalIndent(secretFile{Version: 1, Secrets: secrets}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfg.SecretFile + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfg.SecretFile)
}
