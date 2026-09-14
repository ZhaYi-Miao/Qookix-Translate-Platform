package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Provider 定义供应商凭据与能力，不绑定模型列表（模型在 Chain 中指定）。
type Provider struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // "openai" | "gemini"
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Concurrent int    `json:"concurrent"` // provider 级并发
	TimeoutSec int    `json:"timeout_sec"`

	// PrevName 只出现在请求里：面板改了 provider 名字却没重填 key 时，
	// 服务端靠它找回改名前那条记录的真实 key（保存前会被清空，不落库）。
	PrevName string `json:"prev_name,omitempty"`
}

// ChainStep 是 fallback 链条目，可跨 provider 自由混排。
type ChainStep struct {
	Provider     string `json:"provider"` // 引用 Provider.Name
	ModelName    string `json:"model"`
	DailyLimit   int64  `json:"daily_limit"`        // 0=不限；每 model 每日上限（worker 用）
	OutTokPerMin int64  `json:"out_tokens_per_min"` // 0=不限速；>0 时按该预算对正文分块限速

	// BodyShare 正文分摊权重：>0 的模型参与正文分块分摊（按权重把不同的 mod 分给不同模型），
	// 0 = 不参与分摊、只作为兜底。用于绕开单个 provider 的并发上限。
	BodyShare int `json:"body_share,omitempty"`

	// RPMPerMin 该模型的每分钟请求数上限（0 = 不限）。
	// 有些免费档硬性限制 RPM（例如 20 次/分），超了会被强制冷却一分钟：
	// 与其撞上去吃一次冷却，不如在本地排队，把请求平滑到限速之下。见 main.go 的 modelRPMLimiter。
	RPMPerMin int `json:"rpm,omitempty"`

	// CooldownSec 命中限速后的冷却秒数（0 = 用默认 15s，见 rateLimitCooldownDur）。
	// 对「超限就罚一分钟」的上游，15 秒后重试必然再撞一次，按对方规则配更合适。
	CooldownSec int `json:"rate_limit_cooldown_sec,omitempty"`
}

type RuntimeConfig struct {
	Providers []Provider  `json:"providers"`
	Chain     []ChainStep `json:"chain"`

	BreakerThreshold   int `json:"breaker_threshold"`
	BreakerDurationMin int `json:"breaker_duration_min"`

	// BodyChunkChars 正文分块字符数（0 = 用缺省值 defaultBodyChunkChars）。
	// 调大 => 每次请求更长、调用次数更少；调小 => 切得更碎、并行度更高但调用次数更多。
	BodyChunkChars int `json:"body_chunk_chars,omitempty"`

	// ---- 接口鉴权开关（面板可逐接口切换；缺失的接口用 authmw.go 的默认表）----
	AuthEndpoints map[string]bool `json:"auth_endpoints,omitempty"`

	// ---- AI 管家（顾问）----
	// 用户手动指定"由哪个模型来做运维判断"（不填 = 未启用）。
	// 定位：只读脱敏快照、输出结构化建议，绝不直接改配置。
	AdvisorProvider string `json:"advisor_provider,omitempty"`
	AdvisorModel    string `json:"advisor_model,omitempty"`
	// 观察窗长度（分钟）：管家看到的窗口统计都按这个长度算，并附上一个等长窗口做趋势对比。
	// 免费账号排队白天夜里差别大、分钟内随机抖动，所以这个值由用户按自己的节奏定（默认 10）。
	AdvisorWindowMin int `json:"advisor_window_min,omitempty"`
}

// 掩码哨兵，/config/get 返回时替换 API Key，/config/set 提交时据此保留旧 key。
const maskedAPIKey = "********"

// ============== 熔断器（泛化，按 provider/model 独立实例） ==============

type circuitBreaker struct {
	mu          sync.Mutex
	failCount   int
	brokenUntil time.Time
	threshold   int
	duration    time.Duration
}

func (b *circuitBreaker) isBroken() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.brokenUntil)
}

// remaining 熔断还剩多久（未熔断返回 0），面板用来显示"这个模型现在用不了"。
func (b *circuitBreaker) remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := time.Until(b.brokenUntil); d > 0 {
		return d
	}
	return 0
}

func (b *circuitBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failCount = 0
	b.brokenUntil = time.Time{}
}

func (b *circuitBreaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failCount++
	if b.failCount >= b.threshold {
		b.brokenUntil = time.Now().Add(b.duration)
	}
}

func (b *circuitBreaker) status() (bool, int, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.brokenUntil), b.failCount, b.brokenUntil
}

func (b *circuitBreaker) updateThreshold(threshold int, duration time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if threshold > 0 {
		b.threshold = threshold
	}
	if duration > 0 {
		b.duration = duration
	}
}

// ============== 运行时状态 ==============

var (
	runtimeCfg   *RuntimeConfig
	runtimeCfgMu sync.RWMutex

	provSemMap map[string]chan struct{} // key: provider name; 容量 = Concurrent
	provSemMu  sync.Mutex
	breakers   sync.Map // key: "provider/model" -> *circuitBreaker
)

const configRedisKey = "config:runtime"

func workerParallelism() int {
	n := 0
	for _, p := range getCfg().Providers {
		c := p.Concurrent
		if c < 1 {
			c = 1
		}
		n += c
	}
	return n
}

func findProvider(name string) *Provider {
	return findProviderIn(getCfg(), name)
}

// findProviderIn 在指定配置里按名字找 provider（测试未保存的配置时用它，而不是当前生效配置）。
func findProviderIn(cfg *RuntimeConfig, name string) *Provider {
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			return &cfg.Providers[i]
		}
	}
	return nil
}

// resolveMaskedKeys 把提交上来的空 key / 掩码 key 还原成已保存的真实 key。
// /config/set 保存与 /config/test 测试未保存配置都走这里，避免面板回传掩码时把真实 key 覆盖掉。
// 面板改过 provider 名字时用 PrevName 找回原记录，否则改名会连带把 key 变成掩码。
func resolveMaskedKeys(cfg *RuntimeConfig) {
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.APIKey == "" || p.APIKey == maskedAPIKey {
			lookup := p.Name
			if p.PrevName != "" {
				lookup = p.PrevName
			}
			if old := findProvider(lookup); old != nil {
				p.APIKey = old.APIKey
			}
		}
	}
}

func getProviderSem(name string) chan struct{} {
	provSemMu.Lock()
	defer provSemMu.Unlock()
	return provSemMap[name]
}

func getModelBreaker(provider, model string) *circuitBreaker {
	key := provider + "/" + model
	v, _ := breakers.LoadOrStore(key, &circuitBreaker{
		threshold: 3,
		duration:  5 * time.Minute,
	})
	return v.(*circuitBreaker)
}

func defaultConfig() *RuntimeConfig {
	breakerThreshold, _ := strconv.Atoi(getEnv("BREAKER_THRESHOLD", "3"))
	if breakerThreshold <= 0 {
		breakerThreshold = 3
	}
	breakerDuration, _ := strconv.Atoi(getEnv("BREAKER_DURATION_MIN", "5"))
	if breakerDuration <= 0 {
		breakerDuration = 5
	}
	bodyChunk, _ := strconv.Atoi(getEnv("BODY_CHUNK_CHARS", strconv.Itoa(defaultBodyChunkChars)))
	if bodyChunk <= 0 {
		bodyChunk = defaultBodyChunkChars
	}
	return &RuntimeConfig{
		Providers:          providersFromEnv(),
		Chain:              chainFromEnv(),
		BreakerThreshold:   breakerThreshold,
		BreakerDurationMin: breakerDuration,
		BodyChunkChars:     clampBodyChunkChars(bodyChunk),
	}
}

// bodyChunkSize 返回生效的正文分块大小：配置里是 0（旧配置或没填）就用缺省值。
func bodyChunkSize() int {
	if v := getCfg().BodyChunkChars; v > 0 {
		return clampBodyChunkChars(v)
	}
	return defaultBodyChunkChars
}

// clampBodyChunkChars 把分块大小限制在允许区间内，兜住历史配置里的异常值。
func clampBodyChunkChars(v int) int {
	if v < minBodyChunkChars {
		return minBodyChunkChars
	}
	if v > maxBodyChunkChars {
		return maxBodyChunkChars
	}
	return v
}

// providersFromEnv 从环境变量构造默认 providers（"一个端点跑起来"的极简路径）。
// 只认一个 OpenAI 兼容端点：LLM_API_KEY / LLM_BASE_URL / LLM_CONCURRENT / LLM_TIMEOUT_SEC。
// 多账号、多模型、RPM 限速这些请在面板「模型与配置」里配 —— 那才是推荐用法。
func providersFromEnv() []Provider {
	key := getEnv("LLM_API_KEY", "")
	if key == "" {
		return nil
	}
	conc, _ := strconv.Atoi(getEnv("LLM_CONCURRENT", "2"))
	if conc <= 0 {
		conc = 2
	}
	timeout, _ := strconv.Atoi(getEnv("LLM_TIMEOUT_SEC", "30"))
	if timeout <= 0 {
		timeout = 30
	}
	return []Provider{{
		Name:       "default",
		Type:       "openai",
		BaseURL:    getEnv("LLM_BASE_URL", "https://api.openai.com/v1/chat/completions"),
		APIKey:     key,
		Concurrent: conc,
		TimeoutSec: timeout,
	}}
}

// chainFromEnv 构造默认链：把 LLM_MODELS 里逗号分隔的模型名依次排成兜底链。
func chainFromEnv() []ChainStep {
	if getEnv("LLM_API_KEY", "") == "" {
		return nil
	}
	var chain []ChainStep
	for _, m := range strings.Split(getEnv("LLM_MODELS", ""), ",") {
		if m = strings.TrimSpace(m); m != "" {
			chain = append(chain, ChainStep{Provider: "default", ModelName: m})
		}
	}
	return chain
}

func loadRuntimeConfig() *RuntimeConfig {
	cfg := defaultConfig()
	if redisAlive {
		raw, err := rdb.Get(ctx, configRedisKey).Result()
		if err == nil && raw != "" {
			var stored RuntimeConfig
			if err := json.Unmarshal([]byte(raw), &stored); err == nil {
				// 注意：loadRuntimeConfig 是按字段逐个拷贝的，新字段必须在这里补拷贝，否则重启后丢失
				cfg.AdvisorProvider = stored.AdvisorProvider
				cfg.AdvisorModel = stored.AdvisorModel
				if len(stored.Chain) > 0 {
					// 新格式配置：providers + chain 直接用
					if len(stored.Providers) > 0 {
						cfg.Providers = stored.Providers
					}
					cfg.Chain = stored.Chain
					if stored.BreakerThreshold > 0 {
						cfg.BreakerThreshold = stored.BreakerThreshold
					}
					if stored.BreakerDurationMin > 0 {
						cfg.BreakerDurationMin = stored.BreakerDurationMin
					}
					if stored.BodyChunkChars > 0 {
						cfg.BodyChunkChars = stored.BodyChunkChars
					}
					// 接口鉴权开关（逐接口；缺失的走默认表）
					cfg.AuthEndpoints = stored.AuthEndpoints
					log.Printf("config loaded from redis (new format)")
				} else if len(stored.Providers) > 0 {
					if stored.BreakerThreshold > 0 {
						cfg.BreakerThreshold = stored.BreakerThreshold
					}
					if stored.BreakerDurationMin > 0 {
						cfg.BreakerDurationMin = stored.BreakerDurationMin
					}
					// 有 providers 但没有 chain：给首个 provider 放一个占位模型，等用户在面板里补
					cfg.Providers = stored.Providers
					cfg.Chain = chainFromHeadProvider(stored.Providers)
					log.Printf("config loaded from redis (providers only, chain derived)")
				}
			}
		}
	}
	return cfg
}

// chainFromHeadProvider 仅在没有模型信息时，给首个 provider 放一个占位模型。
func chainFromHeadProvider(ps []Provider) []ChainStep {
	if len(ps) == 0 {
		return nil
	}
	return []ChainStep{{Provider: ps[0].Name, ModelName: "placeholder-model"}}
}

func saveRuntimeConfig(cfg *RuntimeConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if redisAlive {
		return rdb.Set(ctx, configRedisKey, string(raw), 0).Err()
	}
	return fmt.Errorf("redis not available")
}

func getCfg() *RuntimeConfig {
	runtimeCfgMu.RLock()
	defer runtimeCfgMu.RUnlock()
	return runtimeCfg
}

func applyRuntimeConfig(cfg *RuntimeConfig) {
	runtimeCfgMu.Lock()
	runtimeCfg = cfg
	runtimeCfgMu.Unlock()

	// 重建 provider 级信号量
	provSemMu.Lock()
	newSems := make(map[string]chan struct{}, len(cfg.Providers))
	for _, p := range cfg.Providers {
		c := p.Concurrent
		if c < 1 {
			c = 1
		}
		if ch, ok := provSemMap[p.Name]; ok && cap(ch) == c {
			newSems[p.Name] = ch
		} else {
			newSems[p.Name] = make(chan struct{}, c)
		}
	}
	provSemMap = newSems
	provSemMu.Unlock()

	// 重建 model 级熔断表（缺失则建，多余则删，已熔断的保留）
	threshold := cfg.BreakerThreshold
	if threshold <= 0 {
		threshold = 3
	}
	duration := time.Duration(cfg.BreakerDurationMin) * time.Minute
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	valid := map[string]bool{}
	for _, st := range cfg.Chain {
		key := st.Provider + "/" + st.ModelName
		valid[key] = true
		if br, ok := breakers.Load(key); ok {
			br.(*circuitBreaker).updateThreshold(threshold, duration)
		} else {
			breakers.Store(key, &circuitBreaker{threshold: threshold, duration: duration})
		}
	}
	breakers.Range(func(k, _ interface{}) bool {
		if !valid[k.(string)] {
			breakers.Delete(k)
		}
		return true
	})

	// 重建 model 级输出 token 限速器（正文分块限速用），使新预算即时生效
	modelTokLimiters = sync.Map{}
	for _, st := range cfg.Chain {
		_ = modelTokLimiter(st)
	}

	// 同理重建 RPM 限速器：改小上限后必须立刻生效，不能沿用旧桶里攒下的名额。
	// 按账号建桶，同一账号多个模型取链路里的最小 RPM（见 providerRPMLimit 的注释）。
	modelRPMLimiters = sync.Map{}
	for _, st := range cfg.Chain {
		rpm := providerRPMLimit(cfg, st.Provider)
		if rpm <= 0 {
			continue
		}
		modelRPMLimiters.Store(st.Provider, rate.NewLimiter(rate.Limit(rpm/60.0), 1))
	}

	log.Printf("runtime config applied: providers=%d chain=%d breaker=%d/%dm",
		len(cfg.Providers), len(cfg.Chain),
		cfg.BreakerThreshold, cfg.BreakerDurationMin)
}
