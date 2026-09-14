package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestAdvisorLive 实弹测试：用真实配置里的 key 调真实模型，验证
// "真实模型返回 → 识别 → 校验"这一整条链在真实输出上站得住。
//
// 默认跳过。要跑：
//
//	$env:ADVISOR_LIVE='1'; $env:ADVISOR_CFG="$env:TEMP\trans_cfg.json"
//	$env:ADVISOR_PROVIDER='providerF'; $env:ADVISOR_MODEL='model-c'
//	go test -run TestAdvisorLive -v -timeout 300s
//
// 会真实发一次请求（消耗该账号 1 次配额），不会写任何配置。
func TestAdvisorLive(t *testing.T) {
	if os.Getenv("ADVISOR_LIVE") != "1" {
		t.Skip("设置 ADVISOR_LIVE=1 才会真调模型")
	}
	path := os.Getenv("ADVISOR_CFG")
	if path == "" {
		t.Fatal("需要 ADVISOR_CFG 指向一份 RuntimeConfig JSON")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	var cfg RuntimeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}

	wantName := getEnv("ADVISOR_PROVIDER", "providerF")
	model := os.Getenv("ADVISOR_MODEL")
	var p *Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == wantName {
			p = &cfg.Providers[i]
		}
	}
	if p == nil {
		t.Fatalf("配置里没有账号 %s", wantName)
	}
	if model == "" {
		t.Fatalf("需要 ADVISOR_MODEL")
	}

	t.Logf("调用 %s / %s （type=%s，timeout=%ds），提示词 %d 字符，快照 %d 字符",
		p.Name, model, p.Type, p.TimeoutSec, len(advisorSystemPrompt), len(demoSnapshot))

	start := time.Now()
	out, err := advisorCall(*p, model, advisorSystemPrompt, demoSnapshot)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("真实调用失败（%s/%s）: %v", p.Name, model, err)
	}
	t.Logf("=== 真实返回（耗时 %v，%d 字符）===\n%s", elapsed, len(out), out)

	advice, perr := parseAdvisorAdvice(out)
	if perr != nil {
		t.Fatalf("识别失败: %v\n原文:\n%s", perr, out)
	}
	t.Logf("=== 识别结果 ===\n结论: %s\n健康: %s 置信度: %.2f", advice.Summary, advice.Health, advice.Confidence)
	for _, d := range advice.Diagnosis {
		t.Logf("  · %s", d)
	}
	validateAdvisorAdvice(&cfg, advice)
	ok, bad := 0, 0
	for _, c := range advice.Changes {
		if c.Valid {
			ok++
			t.Logf("  可应用: %s.%s %s→%s （%s）", c.Target, c.Field, c.From, c.To, c.Reason)
		} else {
			bad++
			t.Logf("  被拦下: %s.%s=%s → %s", c.Target, c.Field, c.To, c.Error)
		}
	}
	t.Logf("=== 汇总：可应用 %d 条，被护栏拦下 %d 条 ===", ok, bad)
}
