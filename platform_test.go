package main

import (
	"testing"
)

// TestPlatformSupported 只认 Modrinth；其余取值必须判为不支持（请求层据此回 400，
// 也避免把任意字符串写进缓存 key）。
func TestPlatformSupported(t *testing.T) {
	if !platformSupported("modrinth") {
		t.Error("modrinth 应受支持")
	}
	for _, p := range []string{"", "curseforge", "other", "MODRINTH"} {
		if platformSupported(p) {
			t.Errorf("%q 不应受支持", p)
		}
	}
	if got := supportedPlatforms(); len(got) != 1 || got[0] != platformModrinth {
		t.Errorf("supportedPlatforms() = %v，期望只有 modrinth", got)
	}
}

// TestPlatformProjectURL 面板「打开原站」按钮靠它拼链接；未知平台返回空串（前端据此隐藏按钮）。
func TestPlatformProjectURL(t *testing.T) {
	if got := platformProjectURL(platformModrinth, "sodium"); got != "https://modrinth.com/mod/sodium" {
		t.Errorf("modrinth URL 不对: %s", got)
	}
	if got := platformProjectURL("other", "sodium"); got != "" {
		t.Errorf("未知平台应返回空串，得到 %q", got)
	}
}

// TestModRefRoundTrip modRef 是 Worker 任务与正文回访队列的最小单位，
// 序列化成 "platform:id" 存 Redis，解析时要能兼容历史裸 modID。
func TestModRefRoundTrip(t *testing.T) {
	cases := []struct {
		in       string
		platform string
		id       string
	}{
		{"sodium", platformModrinth, "sodium"},                       // 历史裸 modID
		{"modrinth:sodium", platformModrinth, "sodium"},              // 带前缀
		{"curseforge:238222", platformModrinth, "curseforge:238222"}, // 不认识的来源：整串当 Modrinth 标识，不能丢
	}
	for _, c := range cases {
		rt := parseModRef(c.in)
		if rt.Platform != c.platform || rt.ID != c.id {
			t.Errorf("往返失败: %q -> {%s, %s}", c.in, rt.Platform, rt.ID)
		}
	}
	if got := (modRef{Platform: platformModrinth, ID: "sodium"}).String(); got != "modrinth:sodium" {
		t.Errorf("String() = %q", got)
	}
}

// TestPreheatSourceDefaults 预热来源循环：默认只跑 Modrinth，且开关能关掉它。
func TestPreheatSourceDefaults(t *testing.T) {
	old := redisAlive
	redisAlive = false // 走"没有 Redis"的默认分支
	defer func() { redisAlive = old }()

	if !preheatPlatformEnabled(platformModrinth) {
		t.Error("Modrinth 预热应默认开启")
	}
	if got := preheatPlatforms(); len(got) != 1 || got[0] != platformModrinth {
		t.Errorf("默认应只预热 Modrinth，得到 %v", got)
	}

	t.Setenv("WORKER_PREHEAT_MODRINTH", "0")
	if preheatPlatformEnabled(platformModrinth) {
		t.Error("WORKER_PREHEAT_MODRINTH=0 应关掉预热")
	}
	if got := preheatPlatforms(); len(got) != 0 {
		t.Errorf("全部关掉时应返回空，得到 %v", got)
	}
}

// TestPreheatStateShape 面板「后台任务」页依赖这些字段渲染来源控制行，
// 改字段名而忘了改面板会让控制行直接消失（没有编译期保护，只能靠测试锁住）。
func TestPreheatStateShape(t *testing.T) {
	old := redisAlive
	redisAlive = false // 游标都是 0，但字段必须齐全
	defer func() { redisAlive = old }()

	st := preheatState()
	if len(st) != 1 {
		t.Fatalf("preheatState 应只有 1 个来源，得到 %d 个", len(st))
	}
	e, ok := st[platformModrinth]
	if !ok {
		t.Fatal("preheatState 缺少 modrinth 项")
	}
	for _, k := range []string{"enabled", "cursor", "step"} {
		if _, ok := e[k]; !ok {
			t.Errorf("modrinth 状态缺字段 %q", k)
		}
	}
}

// TestWorkerAutoContinueDefault 自动续跑必须默认关闭：它会持续消耗账号额度，
// 只能由面板勾选（或 WORKER_AUTOCONTINUE=1）显式打开。
func TestWorkerAutoContinueDefault(t *testing.T) {
	old := redisAlive
	redisAlive = false
	defer func() { redisAlive = old }()

	if workerAutoContinue() {
		t.Error("自动续跑应默认关闭")
	}
	t.Setenv("WORKER_AUTOCONTINUE", "1")
	if !workerAutoContinue() {
		t.Error("WORKER_AUTOCONTINUE=1 应启用自动续跑")
	}
}
