package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestStepRPMAndLimiter(t *testing.T) {
	// 不填 = 不限速
	if got := stepRPM(ChainStep{Provider: "p", ModelName: "m"}); got != 0 {
		t.Fatalf("未配置时应为 0（不限），得到 %v", got)
	}

	step := ChainStep{Provider: "free", ModelName: "f20", RPMPerMin: 20}
	if got := stepRPM(step); got != 20 {
		t.Fatalf("stepRPM = %v，期望 20", got)
	}
	l := modelRPMLimiter(step)
	if l.Burst() != 1 {
		t.Fatalf("burst 必须为 1（否则短时间突发会把上游打进冷却），得到 %d", l.Burst())
	}
	if want := 20.0 / 60.0; float64(l.Limit()) != want {
		t.Fatalf("rate = %v，期望 %v", float64(l.Limit()), want)
	}

	// 关键性质：同一模型连续两次调用必须被拦住（20/分 => 每 3 秒才放一个）
	if !l.Allow() {
		t.Fatal("第一个请求应该立即放行")
	}
	if l.Allow() {
		t.Fatal("第二个请求不该被放行 —— burst=1 时突发就会撞上游 RPM 限制")
	}

	// 同一账号复用同一个限速器（不能每次调用都新建桶，否则限速形同虚设）
	if again := modelRPMLimiter(step); again != l {
		t.Fatal("同一 provider 应复用同一个限速器实例")
	}
	// 关键：同一账号下的另一个模型必须共用这个桶 ——
	// 上游按账号计 RPM，若按模型各建一个桶，一个账号就会以 N 倍速率超发，照样吃强制冷却。
	same := modelRPMLimiter(ChainStep{Provider: "free", ModelName: "f20b", RPMPerMin: 20})
	if same != l {
		t.Fatal("同一 provider 的不同模型应共用同一个 RPM 桶")
	}
	if same.Allow() {
		t.Fatal("共享桶时第二个请求同样不该被放行")
	}
	// 不同账号互不影响
	other := modelRPMLimiter(ChainStep{Provider: "free2", ModelName: "f20", RPMPerMin: 20})
	if other == l {
		t.Fatal("不同账号不应共用限速器")
	}
	if !other.Allow() {
		t.Fatal("另一个账号的第一个请求应该放行")
	}
}

// TestProviderRPMLimitTakesMin 同一账号挂多个模型时取最小 RPM（保守），
// 并确认应用配置后同账号的两个模型真的共用一个桶。
func TestProviderRPMLimitTakesMin(t *testing.T) {
	cfg := &RuntimeConfig{Chain: []ChainStep{
		{Provider: "a", ModelName: "m1", RPMPerMin: 20},
		{Provider: "a", ModelName: "m2", RPMPerMin: 8}, // 同账号另一模型：应取 8
		{Provider: "b", ModelName: "m3", RPMPerMin: 30},
		{Provider: "c", ModelName: "m4"}, // 未配置 = 不限
	}}
	if got := providerRPMLimit(cfg, "a"); got != 8 {
		t.Fatalf("同账号多模型应取最小值 8，得到 %v", got)
	}
	if got := providerRPMLimit(cfg, "b"); got != 30 {
		t.Fatalf("b 应为 30，得到 %v", got)
	}
	if got := providerRPMLimit(cfg, "c"); got != 0 {
		t.Fatalf("未配置的账号应为 0，得到 %v", got)
	}

	applyRuntimeConfig(cfg)
	l1 := modelRPMLimiter(cfg.Chain[0])
	l2 := modelRPMLimiter(cfg.Chain[1])
	if l1 != l2 {
		t.Fatal("同账号的两个模型应共用同一个限速器")
	}
	if want := 8.0 / 60.0; float64(l1.Limit()) != want {
		t.Fatalf("共享桶速率 = %v，期望 %v（取最小值）", float64(l1.Limit()), want)
	}
}

func TestCooldownFor(t *testing.T) {
	// 缺省 15s，可被步骤覆盖（上游规定"超限罚一分钟"就填 60）
	if got := cooldownFor(ChainStep{}); got != rateLimitCooldownDur {
		t.Fatalf("缺省冷却 = %v，期望 %v", got, rateLimitCooldownDur)
	}
	if got := cooldownFor(ChainStep{CooldownSec: 60}); got != 60*time.Second {
		t.Fatalf("覆盖冷却 = %v，期望 60s", got)
	}

	key := "free/f20"
	setRateLimitCooldownFor(key, 60*time.Second)
	left := rateLimitCooling(key)
	if left <= 55*time.Second || left > 60*time.Second {
		t.Fatalf("冷却剩余 = %v，期望接近 60s", left)
	}
	clearRateLimitCooldown(key)
	if left := rateLimitCooling(key); left != 0 {
		t.Fatalf("清理后应无冷却，得到 %v", left)
	}
}

// TestCallChainRespectsRPM 走真实的 callChainIn 路径：配了 RPM 之后，第二个请求必须排队等到
// 60/RPM 秒之后才发出去。这里用 120/分（0.5s 间隔）让测试保持在 1 秒内。
func TestCallChainRespectsRPM(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	cfg := &RuntimeConfig{
		Providers: []Provider{{Name: "rpmtest", Type: "openai", BaseURL: srv.URL, Concurrent: 2, TimeoutSec: 5}},
		Chain:     []ChainStep{{Provider: "rpmtest", ModelName: "m", RPMPerMin: 120}},
	}
	applyRuntimeConfig(cfg)

	for i := 0; i < 2; i++ {
		got, err := callChainIn(cfg, "hello", "zh", cfg.Chain, false, "test")
		if err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
		if got != "ok" {
			t.Fatalf("返回值 = %q", got)
		}
	}
	if len(stamps) != 2 {
		t.Fatalf("上游收到 %d 次请求，期望 2 次", len(stamps))
	}
	gap := stamps[1].Sub(stamps[0])
	if gap < 400*time.Millisecond {
		t.Fatalf("两次请求间隔 %v，RPM 限速没生效（期望 >=400ms）", gap)
	}
	if gap > 2*time.Second {
		t.Fatalf("两次请求间隔 %v，排队过久（期望 ~500ms）", gap)
	}
}

// TestRPMLimiterRebuiltOnConfigChange 改配置后重建限速器：把 RPM 改小必须立刻生效，
// 不能沿用旧桶里攒下的名额（否则改完还会先超发一批）。
func TestRPMLimiterRebuiltOnConfigChange(t *testing.T) {
	cfg := &RuntimeConfig{
		Providers: []Provider{{Name: "p1", Type: "openai", BaseURL: "http://x", Concurrent: 1, TimeoutSec: 5}},
		Chain:     []ChainStep{{Provider: "p1", ModelName: "m1", RPMPerMin: 600}},
	}
	applyRuntimeConfig(cfg)
	big := modelRPMLimiter(cfg.Chain[0])
	big.Allow() // 先用掉 burst 里的名额

	cfg2 := &RuntimeConfig{
		Providers: cfg.Providers,
		Chain:     []ChainStep{{Provider: "p1", ModelName: "m1", RPMPerMin: 20}},
	}
	applyRuntimeConfig(cfg2)
	small := modelRPMLimiter(cfg2.Chain[0])
	if float64(small.Limit()) != 20.0/60.0 {
		t.Fatalf("改配置后 rate = %v，期望 %v", float64(small.Limit()), 20.0/60.0)
	}
	if small == big {
		t.Fatal("改配置后应重建限速器")
	}
}
