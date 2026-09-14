package main

import (
	"strings"
	"testing"
)

// TestParseAdvisorAdvice 容忍 ```json 包裹与前后废话，能抠出 JSON。
func TestParseAdvisorAdvice(t *testing.T) {
	cases := []string{
		`{"summary":"ok","health":"good","changes":[]}`,
		"```json\n{\"summary\":\"ok\",\"health\":\"warn\",\"changes\":[]}\n```",
		"好的，我的建议如下：\n{\"summary\":\"ok\",\"health\":\"bad\",\"changes\":[]}\n以上。",
	}
	for i, raw := range cases {
		a, err := parseAdvisorAdvice(raw)
		if err != nil {
			t.Fatalf("第 %d 个用例解析失败: %v", i+1, err)
		}
		if a.Summary != "ok" {
			t.Errorf("第 %d 个用例 summary = %q", i+1, a.Summary)
		}
	}
	if _, err := parseAdvisorAdvice("完全没有 JSON"); err == nil {
		t.Error("没有 JSON 时应该报错")
	}
}

// TestValidateAdvisorAdvice 护栏：非法字段/越界值/不存在的账号/链路顺序被改动都要被标出来。
func TestValidateAdvisorAdvice(t *testing.T) {
	cfg := &RuntimeConfig{
		Providers: []Provider{{Name: "A1"}, {Name: "G1"}},
		Chain:     []ChainStep{{Provider: "A1", ModelName: "m"}, {Provider: "G1", ModelName: "m"}},
	}
	a := &advisorAdvice{Changes: []advisorChange{
		{Target: "A1", Field: "concurrent", To: "2"},               // 合法
		{Target: "A1", Field: "concurrent", To: "0"},               // 越界
		{Target: "A1", Field: "concurrent", To: "999"},             // 越界
		{Target: "NOPE", Field: "concurrent", To: "2"},             // 账号不存在
		{Target: "A1", Field: "delete_everything", To: "1"},        // 非法字段
		{Target: "A1", Field: "chain_order", To: "A1"},             // 少了一个账号
		{Target: "A1", Field: "chain_order", To: "G1, A1"},         // 只是调序：合法
		{Target: "A1", Field: "rpm", To: "20"},                     // 合法
		{Target: "A1", Field: "rate_limit_cooldown_sec", To: "60"}, // 合法
	}}
	validateAdvisorAdvice(cfg, a)

	want := []bool{true, false, false, false, false, false, true, true, true}
	for i, w := range want {
		if a.Changes[i].Valid != w {
			t.Errorf("第 %d 条 valid = %v，期望 %v（err=%q）", i+1, a.Changes[i].Valid, w, a.Changes[i].Error)
		}
	}
}

// TestAdvisorRedact 样本脱敏：request id / 链接 / key 必须去掉，错误原文要保留。
func TestAdvisorRedact(t *testing.T) {
	in := "providerD error: 您已达到免费用户的 API 速率限制。升级 Token Plan (request id: NZ3M07mpG) 见 https://api.example.com/x sk-ABC123456789"
	got := advisorRedact(in)
	for _, bad := range []string{"NZ3M07mpG", "https://", "sk-ABC123456789"} {
		if strings.Contains(got, bad) {
			t.Errorf("脱敏后仍包含 %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "速率限制") {
		t.Errorf("错误原文应该保留（判断处置要用）: %s", got)
	}
}

// TestAdvisorCategorize 错误归类。
func TestAdvisorCategorize(t *testing.T) {
	cases := map[string]string{
		"您已达到免费用户的 API 速率限制":        "rate_limit",
		"429 Too Many Requests":     "rate_limit",
		"context deadline exceeded": "timeout",
		"read tcp: i/o timeout":     "timeout",
		"401 unauthorized":          "auth",
		"账户余额不足":                    "quota",
		"502 Bad Gateway":           "server",
		"something weird":           "other",
	}
	for msg, want := range cases {
		if got := advisorCategorize(msg); got != want {
			t.Errorf("advisorCategorize(%q) = %q，期望 %q", msg, got, want)
		}
	}
}
