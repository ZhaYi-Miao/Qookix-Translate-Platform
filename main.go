package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
	_ "modernc.org/sqlite"
)

var (
	rdb        *redis.Client
	db         *sql.DB
	ctx        = context.Background()
	sf         singleflight.Group
	redisAlive bool
)

const (
	maxBatchSize = 5
	maxBodyBytes = 64 << 10

	// maxBodyTranslateChars 单次 body 翻译的上限字符数，避免超长正文产生过高成本/超时。
	maxBodyTranslateChars = 12000

	// 正文分块大小可在面板配置（body_chunk_chars）；这里是缺省值与允许区间。
	defaultBodyChunkChars = 1500
	minBodyChunkChars     = 200
	maxBodyChunkChars     = 8000

	// bodyChunkInflight 单篇正文同时在飞的请求数上限。
	// 一篇 12000 字符的正文有 8 块，全部一起发会在同一秒把账号的每分钟额度打满 → 429 →
	// 冷却 → 级联限速，这是"正文回访失败率 80%+"的直接原因（7 个 worker 槽 × 8 块 = 几十个并发）。
	bodyChunkInflight = 2

	// 正文失败分块的重试：只重翻失败的那几块，并退避等待（限速是分钟级恢复的）。
	// 整篇重做会把已经翻好的块再翻一遍，白烧一遍额度、还会加重限速。
	bodyChunkRetryTimes = 2
	bodyChunkRetryDelay = 8 * time.Second

	// 每分钟输出 token 预算（out_tokens_per_min）语义：0 = 不限速（与每日上限 0=不限 一致）；
	// 填具体数值才按该预算对正文分块限速，用于防止打爆有 RPM/PPM 限制的模型（如 Gemini 免费档）。

	// outTokFactor 输入字符 → 估算 token 的换算系数（保守取 1.0=每字符约 1 token）。
	outTokFactor = 1.0

	// tokWaitTimeout 等待某个 model 的 token 预算时的最长排队时间，超时则跳过该 model 走兜底。
	// 链尾兜底步骤的预算一旦被「限流回落」的请求抽干，后续请求会一直排在这里；
	// 实测有 mod 因此占用 worker 槽位 20 分钟，而且等到最后依然是失败（白等）。
	// 预算排队只做短暂让路，超时立刻换下一个模型。
	tokWaitTimeout = 20 * time.Second

	// providerSemWaitTimeout 等某个 provider 并发槽的最长时间。
	// 单个请求一旦撞上 30s 超时，账号的 2 个并发槽就会被占死，后面的请求白等 100 秒以上
	// （实测 semwait=113.2s 之后照样超时），把 worker 槽位长期拖住；等不到就直接换下一个 model。
	providerSemWaitTimeout = 20 * time.Second

	// rpmWaitTimeout 等某个 model 的 RPM 名额的最长时间。
	// 配了 RPM 的模型（如免费档 20 次/分 => 每 3 秒一次）在繁忙时会排几秒队，
	// 等一会儿远比撞限速吃冷却划算；但排太久就把 worker 槽位浪费了，超时直接走下一个 model。
	rpmWaitTimeout = 20 * time.Second

	translateRate  = rate.Limit(30.0 / 60.0)
	translateBurst = 10

	feedbackRate  = rate.Limit(10.0 / 86400.0)
	feedbackBurst = 3

	qualityRate  = rate.Limit(5.0 / 86400.0)
	qualityBurst = 2

	modrinthRate  = rate.Limit(200.0 / 60.0)
	modrinthBurst = 20

	cooldownDuration = 24 * time.Hour
	cacheTTL         = 30 * 24 * time.Hour
	maxSuggestionLen = 500
)

var modrinthLimiter = rate.NewLimiter(modrinthRate, modrinthBurst)

// ============== 数据结构 ==============

type TranslateRequest struct {
	Platform    string `json:"platform"`
	ModID       string `json:"mod_id"`
	Lang        string `json:"lang"`
	IncludeBody bool   `json:"include_body,omitempty"`
}

type TranslateResponse struct {
	Text       string `json:"text,omitempty"`
	Cached     bool   `json:"cached"`
	Body       string `json:"body,omitempty"`
	BodyCached bool   `json:"body_cached,omitempty"`
	Error      string `json:"error,omitempty"`
}

type BatchRequest struct {
	Platform    string   `json:"platform"`
	Lang        string   `json:"lang"`
	ModIDs      []string `json:"mod_ids"`
	IncludeBody bool     `json:"include_body,omitempty"`
}

type BatchResponse struct {
	Results map[string]TranslateResponse `json:"results"`
}

type FeedbackRequest struct {
	Platform string `json:"platform"`
	ModID    string `json:"mod_id"`
	Lang     string `json:"lang"`
}

type QualityFeedbackRequest struct {
	Platform       string `json:"platform"`
	ModID          string `json:"mod_id"`
	Lang           string `json:"lang"`
	IssueType      string `json:"issue_type"`
	UserSuggestion string `json:"user_suggestion"`
	UserComment    string `json:"user_comment"`
}

type FeedbackResponse struct {
	Status string `json:"status"`
}

type CacheEntry struct {
	Text         string `json:"text"`
	Updated      string `json:"updated"`
	TranslatedAt int64  `json:"translated_at"`
	// Fmt 缓存内容的格式版本：2 = 正文经 Markdown 转换器产出。
	// 旧条目没有该字段（反序列化后为 0），「CF 正文格式升级」批量任务据此判定哪些需要重译。
	Fmt int `json:"fmt,omitempty"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type ModrinthProject struct {
	Description string `json:"description"`
	Body        string `json:"body"`
	Title       string `json:"title"`
	Updated     string `json:"updated"`
}

// ============== 限流器 ==============

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type limiterStore struct {
	mu       sync.Mutex
	limiters map[string]*ipLimiter
	r        rate.Limit
	b        int
}

func newLimiterStore(r rate.Limit, b int) *limiterStore {
	s := &limiterStore{
		limiters: make(map[string]*ipLimiter),
		r:        r,
		b:        b,
	}
	go s.cleanupLoop()
	return s
}

func (s *limiterStore) get(ip string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.limiters[ip]
	if !ok {
		lim := rate.NewLimiter(s.r, s.b)
		s.limiters[ip] = &ipLimiter{limiter: lim, lastSeen: time.Now()}
		return lim
	}
	item.lastSeen = time.Now()
	return item.limiter
}

func (s *limiterStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		s.mu.Lock()
		for ip, item := range s.limiters {
			if time.Since(item.lastSeen) > 10*time.Minute {
				delete(s.limiters, ip)
			}
		}
		s.mu.Unlock()
	}
}

var translateLimiters *limiterStore
var feedbackLimiters *limiterStore
var qualityLimiters *limiterStore

// ============== main ==============

func main() {
	redisAddr := getEnv("REDIS_ADDR", "127.0.0.1:6380")
	redisPass := getEnv("REDIS_PASS", "")
	port := getEnv("PORT", "8080")
	sqlitePath := getEnv("SQLITE_PATH", "./cache.db")

	if getEnv("LLM_API_KEY", "") == "" {
		log.Fatal("no LLM API key configured (set LLM_API_KEY, or configure providers in the panel)")
	}

	initWorkerLogger()

	rdb = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPass,
		DB:       0,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("WARN: redis connect failed: %v, falling back to SQLite only", err)
		redisAlive = false
	} else {
		log.Printf("redis connected: %s", redisAddr)
		redisAlive = true
	}

	var err error
	db, err = sql.Open("sqlite", sqlitePath)
	if err != nil {
		log.Fatalf("sqlite open failed: %v", err)
	}
	if err := initSQLite(); err != nil {
		log.Fatalf("sqlite init failed: %v", err)
	}
	log.Printf("sqlite ready: %s", sqlitePath)

	applyRuntimeConfig(loadRuntimeConfig())
	cfg := getCfg()
	provNames := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		provNames = append(provNames, p.Name)
	}
	chainModels := make([]string, 0, len(cfg.Chain))
	for _, st := range cfg.Chain {
		chainModels = append(chainModels, st.Provider+"/"+st.ModelName)
	}
	log.Printf("providers: %v", provNames)
	log.Printf("fallback chain: %v", chainModels)
	log.Printf("breaker: %d failures -> %dm", cfg.BreakerThreshold, cfg.BreakerDurationMin)
	log.Printf("worker parallelism: %d", workerParallelism())
	log.Printf("modrinth rate: %.0f/min", modrinthRate*60)

	translateLimiters = newLimiterStore(translateRate, translateBurst)
	feedbackLimiters = newLimiterStore(feedbackRate, feedbackBurst)
	qualityLimiters = newLimiterStore(qualityRate, qualityBurst)

	// 服务端吞吐采样：面板的"翻译速度"用这里的权威口径，不再靠前端轮询差值估算
	startThroughputSampler()

	// 管家窗口采样：按分钟归桶，让快照能给出任意窗口 + 上一窗口趋势
	// （免费账号排队波动大，单点数值没法作为调整依据）
	startAdvisorBucketSampler()

	// 所有接口统一过 guardAPI：是否需要 Basic 认证由面板逐接口开关决定（authmw.go）
	http.HandleFunc("/health", guardAPI(handleHealth))
	// 对外只读状态（文档站状态页用，默认公开、带 CORS 与 15s 缓存）
	http.HandleFunc("/public/status", guardAPI(handlePublicStatus))
	http.HandleFunc("/public/uptime", guardAPI(handlePublicUptime))
	http.HandleFunc("/public/incidents", guardAPI(handlePublicIncidents))
	http.HandleFunc("/translate/mod", guardAPI(rateLimitMiddleware(translateLimiters, 1, handleTranslateMod)))
	http.HandleFunc("/translate/mods", guardAPI(handleTranslateMods))
	http.HandleFunc("/feedback/stale", guardAPI(handleFeedbackStale))
	http.HandleFunc("/feedback/quality", guardAPI(handleFeedbackQuality))
	http.HandleFunc("/stats", guardAPI(handleStats))
	http.HandleFunc("/worker/toggle", guardAPI(handleWorkerToggle))
	http.HandleFunc("/worker/start", guardAPI(handleWorkerStart))
	http.HandleFunc("/worker/stop", guardAPI(handleWorkerStop))
	http.HandleFunc("/worker/status", guardAPI(handleWorkerStatus))
	http.HandleFunc("/worker/preheat/cursor", guardAPI(handlePreheatCursor))
	http.HandleFunc("/worker/logs", guardAPI(handleWorkerLogs))
	http.HandleFunc("/worker/feedback/list", guardAPI(handleFeedbackList))
	http.HandleFunc("/worker/feedback/status", guardAPI(handleFeedbackStatus))
	http.HandleFunc("/worker/feedback/retranslate", guardAPI(handleFeedbackRetranslate))
	http.HandleFunc("/worker/cache/list", guardAPI(handleCacheList))
	http.HandleFunc("/worker/cache/get", guardAPI(handleCacheGet))
	// AI 管家：脱敏快照 / 让指定模型给建议 / 回显上次建议（默认都需要鉴权）
	http.HandleFunc("/advisor/snapshot", guardAPI(handleAdvisorSnapshot))
	http.HandleFunc("/advisor/ask", guardAPI(handleAdvisorAsk))
	http.HandleFunc("/advisor/last", guardAPI(handleAdvisorLast))
	http.HandleFunc("/advisor/apply", guardAPI(handleAdvisorApply))
	http.HandleFunc("/advisor/rollback", guardAPI(handleAdvisorRollback))
	// 实时槽位视图：每个账号的并发槽位此刻在干什么、有多少请求卡在拿名额
	http.HandleFunc("/live", guardAPI(handleLive))
	http.HandleFunc("/config/get", guardAPI(handleConfigGet))
	http.HandleFunc("/config/set", guardAPI(handleConfigSet))
	http.HandleFunc("/config/reset", guardAPI(handleConfigReset))
	http.HandleFunc("/config/test", guardAPI(handleConfigTest))

	go resumeWorkerIfNeeded()

	addr := "127.0.0.1:" + port
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

// ============== 限流中间件 ==============

func rateLimitMiddleware(store *limiterStore, weight int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := getClientIP(r)
		statIP(ip)
		lim := store.get(ip)
		for i := 0; i < weight; i++ {
			if !lim.Allow() {
				statRateLimitHit()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"rate limit exceeded"}`))
				log.Printf("rate limit hit: ip=%s path=%s", ip, r.URL.Path)
				return
			}
		}
		next(w, r)
	}
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		if idx := strings.Index(ip, ","); idx != -1 {
			ip = ip[:idx]
		}
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ============== SQLite ==============

func initSQLite() error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS translations (
			key             TEXT PRIMARY KEY,
			text            TEXT NOT NULL,
			updated_at      INTEGER NOT NULL,
			last_checked_at INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS quality_feedback (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			platform        TEXT NOT NULL,
			mod_id          TEXT NOT NULL,
			lang            TEXT NOT NULL,
			issue_type      TEXT NOT NULL,
			user_suggestion TEXT,
			user_comment    TEXT,
			ip              TEXT,
			created_at      INTEGER NOT NULL,
			status          TEXT NOT NULL DEFAULT 'pending'
		);
	`)
	if err != nil {
		return err
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('translations') WHERE name = 'last_checked_at'`).Scan(&count)
	if count == 0 {
		log.Printf("migrating: adding last_checked_at column")
		if _, err := db.Exec(`ALTER TABLE translations ADD COLUMN last_checked_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migration failed: %v", err)
		}
	}

	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_translations_last_checked ON translations(last_checked_at)`); err != nil {
		return fmt.Errorf("create index failed: %v", err)
	}
	return nil
}

func sqliteGet(key string) (string, bool) {
	var text string
	err := db.QueryRow(`SELECT text FROM translations WHERE key = ?`, key).Scan(&text)
	if err != nil {
		return "", false
	}
	return text, true
}

func sqliteSet(key, text string) error {
	_, err := db.Exec(`
		INSERT INTO translations (key, text, updated_at, last_checked_at)
		VALUES (?, ?, ?, 0)
		ON CONFLICT(key) DO UPDATE SET
			text = excluded.text,
			updated_at = excluded.updated_at
	`, key, text, time.Now().Unix())
	return err
}

func sqliteDelete(key string) error {
	_, err := db.Exec(`DELETE FROM translations WHERE key = ?`, key)
	return err
}

// ============== 缓存 ==============

func cacheGetRaw(key string) (string, bool) {
	if redisAlive {
		if val, err := rdb.Get(ctx, key).Result(); err == nil {
			return val, true
		}
	}
	if val, ok := sqliteGet(key); ok {
		if redisAlive {
			_ = rdb.Set(ctx, key, val, cacheTTL).Err()
		}
		return val, true
	}
	return "", false
}

func cacheSetRaw(key, val string) {
	if redisAlive {
		if err := rdb.Set(ctx, key, val, cacheTTL).Err(); err != nil {
			log.Printf("redis set error: %v", err)
		}
	}
	if err := sqliteSet(key, val); err != nil {
		log.Printf("sqlite set error: %v", err)
	}
}

func cacheDelete(key string) {
	if redisAlive {
		_ = rdb.Del(ctx, key).Err()
	}
	_ = sqliteDelete(key)
}

func cacheGetEntry(key string) (*CacheEntry, bool) {
	raw, ok := cacheGetRaw(key)
	if !ok {
		return nil, false
	}
	var entry CacheEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return &CacheEntry{Text: raw}, true
	}
	return &entry, true
}

// cacheHasText 判断某个缓存键是否已有可用译文。
func cacheHasText(key string) bool {
	entry, ok := cacheGetEntry(key)
	return ok && entry.Text != ""
}

func cacheSetEntry(key string, entry CacheEntry) {
	raw, err := json.Marshal(entry)
	if err != nil {
		log.Printf("marshal cache entry failed: %v", err)
		return
	}
	cacheSetRaw(key, string(raw))
}

// ============== 核心翻译 ==============

func modCacheKey(platform, modID, lang string) string {
	return fmt.Sprintf("trans:mod:%s:%s:%s", platform, modID, lang)
}

// getCached 是「单飞 + 缓存记账」的唯一实现：命中直接返回；未命中时同 key 只允许一个 goroutine 真正翻译，
// 其余并发请求共享这次结果，并按「没有付出翻译成本」记为命中。
// 记账放在单飞函数体内部，避免一次翻译被并发的同 key 请求重复记成多次未命中。
// kind 为统计维度（desc/body），translate 负责取数、调用 LLM 并写缓存。
func getCached(cacheKey, kind string, translate func() (string, error)) (string, bool, error) {
	if entry, ok := cacheGetEntry(cacheKey); ok && entry.Text != "" {
		statCacheHit()
		statCacheKind(kind, true)
		return entry.Text, true, nil
	}

	didWork, servedFromCache := false, false
	v, err, _ := sf.Do(cacheKey, func() (interface{}, error) {
		didWork = true
		if entry, ok := cacheGetEntry(cacheKey); ok && entry.Text != "" {
			servedFromCache = true
			statCacheHit()
			statCacheKind(kind, true)
			return entry.Text, nil
		}
		statCacheMiss()
		statCacheKind(kind, false)
		s, terr := translate()
		if terr != nil {
			return nil, terr
		}
		return s, nil
	})
	if err != nil {
		return "", false, err
	}
	if !didWork {
		servedFromCache = true
		statCacheHit()
		statCacheKind(kind, true)
	}
	return v.(string), servedFromCache, nil
}

func getModTranslation(platform, modID, lang string) (string, bool, error) {
	// Modrinth 的标识（slug / nanoid）本身就是稳定主键，直接当缓存键的一部分用。
	statMod(modID)
	return getCached(modCacheKey(platform, modID, lang), "desc", func() (string, error) {
		return translateAndCache(platform, modID, lang)
	})
}

// llmCall 是翻译调用的统一签名：客户端传 callLLM（不限预算），
// Worker 传 callChainForWorker（逐 model 校验每日预算）。
type llmCall func(text, lang string) (string, error)

// errEmptyDesc 表示 Modrinth 侧没有描述内容；重试没有意义，Worker 依据它跳过重试。
var errEmptyDesc = errors.New("empty description")

// errContentRejected 表示上游以「内容审核」为由拒绝了这段文本。
// 同一套审核策略换个账号照样会拒，所以既不重试分块、也不进正文回访队列（否则每轮白烧额度）。
var errContentRejected = errors.New("content rejected by upstream")

// isContentRejectError 判断错误是否来自上游内容审核。
func isContentRejectError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{
		"不安全或敏感内容", "敏感内容", "content filter", "content_filter", "data_inspection_failed",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// translateAndCache 客户端描述翻译入口：不做每日预算校验。
func translateAndCache(platform, modID, lang string) (string, error) {
	return translateDescAndCache(platform, modID, lang, callLLM)
}

// translateDescAndCache 拉取描述 -> 调用 LLM -> 写缓存。
// 已经拿到原文的场景（如 Worker 一次拉取同时翻描述和正文）请直接用 translateAndCacheDesc，避免重复请求 Modrinth。
func translateDescAndCache(platform, modID, lang string, call llmCall) (string, error) {
	desc, _, updated, err := fetchModFullByPlatform(platform, modID)
	if err != nil {
		return "", fmt.Errorf("fetch %s failed: %v", platform, err)
	}
	return translateAndCacheDesc(platform, modID, lang, desc, updated, call)
}

// translateAndCacheDesc 把已取到的描述翻译并写缓存，并记录 desc 维度的调用统计。
// 描述为空时返回 errEmptyDesc（Worker 据此跳过、不重试）。
func translateAndCacheDesc(platform, modID, lang, desc, updated string, call llmCall) (string, error) {
	if desc == "" {
		return "", fmt.Errorf("%w for %s", errEmptyDesc, modID)
	}

	translated, err := call(desc, lang)
	statKindCall("desc", err == nil, int64(len(desc)))
	if err != nil {
		return "", err
	}
	translated = tidyTranslation(translated)

	entry := CacheEntry{
		Text:         translated,
		Updated:      updated,
		TranslatedAt: time.Now().Unix(),
	}
	cacheSetEntry(modCacheKey(platform, modID, lang), entry)
	return translated, nil
}

// manyNewlines 匹配 3 个以上连续换行。
var manyNewlines = regexp.MustCompile(`\n{3,}`)

// tidyTranslation 规整模型译文：去掉首尾空白、把 3 个以上连续换行折成 2 个。
// 模型经常在开头多吐一个空行，Markdown 渲染时会变成顶部一大块空白（面板上很明显）。
func tidyTranslation(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	return manyNewlines.ReplaceAllString(s, "\n\n")
}

func modBodyCacheKey(platform, modID, lang string) string {
	return fmt.Sprintf("trans:mod:body:%s:%s:%s", platform, modID, lang)
}

func getBodyTranslation(platform, modID, lang string) (string, bool, error) {
	// statMod 只在 desc 入口记一次：正文翻译必定伴随描述翻译，
	// 两处都记会让同一个请求把该 mod 的热度算两遍。
	return getCached(modBodyCacheKey(platform, modID, lang), "body", func() (string, error) {
		return translateBodyAndCache(platform, modID, lang, getCfg(), false)
	})
}

// translateBodyAndCache 拉取正文 -> 分块翻译 -> 写缓存。
// checkBudget=true 时按链上每日预算跳过超限模型（Worker 入口），false 为客户端入口。
func translateBodyAndCache(platform, modID, lang string, cfg *RuntimeConfig, checkBudget bool) (string, error) {
	_, body, updated, err := fetchModFullByPlatform(platform, modID)
	if err != nil {
		return "", fmt.Errorf("fetch %s failed: %v", platform, err)
	}
	return translateAndCacheBody(platform, modID, lang, body, updated, cfg, checkBudget)
}

// translateAndCacheBody 把已取到的正文截断后分块翻译并写缓存，并记录 body 维度的调用统计。
func translateAndCacheBody(platform, modID, lang, body, updated string, cfg *RuntimeConfig, checkBudget bool) (string, error) {
	if body == "" {
		return "", fmt.Errorf("empty body for %s", modID)
	}
	text := body
	if len(text) > maxBodyTranslateChars {
		text = text[:maxBodyTranslateChars] + "\n\n[正文过长，已截断翻译]"
		log.Printf("body truncated: mod=%s len=%d -> %d", modID, len(body), len(text))
	}

	translated, err := translateBodyChunked(cfg, modID, text, lang, checkBudget)
	statKindCall("body", err == nil, int64(len(text)))
	if err != nil {
		return "", err
	}
	translated = tidyTranslation(translated)

	entry := CacheEntry{
		Text:         translated,
		Updated:      updated,
		TranslatedAt: time.Now().Unix(),
		Fmt:          cacheFmtMarkdown,
	}
	cacheSetEntry(modBodyCacheKey(platform, modID, lang), entry)
	return translated, nil
}

// ============== 正文分块并行翻译 + 输出 Token 限速 ==============

// modelTokLimiters 按 "provider/model" 维护每分钟输出 token 限速器。
var modelTokLimiters sync.Map

// stepOutBudget 返回链步骤的每分钟输出 token 预算；<=0 表示不限速（与每日上限 0=不限 一致）。
func stepOutBudget(st ChainStep) float64 {
	if st.OutTokPerMin > 0 {
		return float64(st.OutTokPerMin)
	}
	return 0
}

// modelTokLimiter 获取（懒创建）某 provider/model 的每分钟输出 token 限速器。
func modelTokLimiter(step ChainStep) *rate.Limiter {
	key := step.Provider + "/" + step.ModelName
	if v, ok := modelTokLimiters.Load(key); ok {
		return v.(*rate.Limiter)
	}
	perSec := stepOutBudget(step) / 60.0
	l := rate.NewLimiter(rate.Limit(perSec), int(perSec*2))
	actual, _ := modelTokLimiters.LoadOrStore(key, l)
	return actual.(*rate.Limiter)
}

// ============== 每分钟请求数（RPM）限速 ==============

// modelRPMLimiters 按 **provider（= 一个账号/一个 key）** 维护每分钟请求数限速器。
//
// 为什么按账号而不是 provider+模型：上游的速率限制几乎都按 key/账号计
// （免费档常直接返回「您已达到免费用户的 API 速率限制」），
// 同一账号下挂多个模型时，它们共用同一份额度。若按模型各建一个桶，
// 同一个账号就会以 N 倍的速率往外发请求，照样撞限速、照样吃强制冷却。
var modelRPMLimiters sync.Map

// stepRPM 返回链步骤的每分钟请求上限；<=0 表示不限。
func stepRPM(st ChainStep) float64 {
	if st.RPMPerMin > 0 {
		return float64(st.RPMPerMin)
	}
	return 0
}

// providerRPMLimit 取某账号在链路里配置的最小 RPM：同一账号挂了多个模型时，
// 取最小值是保守做法（宁可慢一点，也不要因为多模型各算一份而超发）。
func providerRPMLimit(cfg *RuntimeConfig, provider string) float64 {
	min := 0.0
	for _, st := range cfg.Chain {
		if st.Provider != provider {
			continue
		}
		if v := stepRPM(st); v > 0 && (min == 0 || v < min) {
			min = v
		}
	}
	return min
}

// modelRPMLimiter 获取（懒创建）某 provider（账号）的 RPM 限速器。
//
// burst 固定为 1 是关键：burst>1 时可以在几秒内连打一批请求，长均值虽然还是满足 RPM，
// 但上游是按滑动窗口计数的（典型规则「每分钟 20 次，超了强制冷却一分钟」），
// 一次突发就足以把账号打进冷却。burst=1 让请求严格按 60/RPM 秒的间隔发出去。
func modelRPMLimiter(step ChainStep) *rate.Limiter {
	key := step.Provider
	if v, ok := modelRPMLimiters.Load(key); ok {
		return v.(*rate.Limiter)
	}
	l := rate.NewLimiter(rate.Limit(stepRPM(step)/60.0), 1)
	actual, _ := modelRPMLimiters.LoadOrStore(key, l)
	return actual.(*rate.Limiter)
}

// cooldownFor 返回该链步骤命中限速后的冷却时长：可配，缺省 rateLimitCooldownDur。
func cooldownFor(st ChainStep) time.Duration {
	if st.CooldownSec > 0 {
		return time.Duration(st.CooldownSec) * time.Second
	}
	return rateLimitCooldownDur
}

// splitBody 按段落/换行把正文切块，每块尽量不超过 chunkChars 字符。
// 空行保留为段落分隔，否则 Markdown 里被空行隔开的段落会被合并成一段。
func splitBody(body string, chunkChars int) []string {
	if len(body) <= chunkChars {
		return []string{body}
	}
	var chunks []string
	cur := strings.Builder{}
	for _, para := range strings.Split(body, "\n") {
		for {
			if cur.Len() > 0 && cur.Len()+len(para)+1 > chunkChars {
				chunks = append(chunks, cur.String())
				cur.Reset()
			}
			if len(para) <= chunkChars {
				if cur.Len() > 0 {
					cur.WriteRune('\n')
				}
				cur.WriteString(para)
				break
			}
			// 单段超长：硬切
			part := para[:chunkChars]
			chunks = append(chunks, part)
			para = para[chunkChars:]
		}
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

// tokWait 按每次不超过 burst 的方式等待 n 个 token，严格遵循每分钟输出预算。
func tokWait(ctx context.Context, l *rate.Limiter, n int) error {
	burst := l.Burst()
	if burst < 1 {
		burst = 1
	}
	for n > 0 {
		step := n
		if step > burst {
			step = burst
		}
		if err := l.WaitN(ctx, step); err != nil {
			return err
		}
		n -= step
	}
	return nil
}

// bodyChain 返回该 mod 正文分块使用的尝试顺序：按 body_share 权重把 mod 稳定地分给其中一个模型
// 并排到最前，其余模型按原顺序跟在后面兜底；没有任何模型配置权重时保持原链顺序（等于只用链首）。
func bodyChain(cfg *RuntimeConfig, modID string) []ChainStep {
	var picked []ChainStep
	total := 0
	for _, st := range cfg.Chain {
		if st.BodyShare > 0 {
			picked = append(picked, st)
			total += st.BodyShare
		}
	}
	if total == 0 {
		return cfg.Chain
	}

	// 用 mod 名做稳定散列：同一个 mod 每次都落在同一个模型上，重试/重跑结果一致
	h := 0
	for i := 0; i < len(modID); i++ {
		h = (h*31 + int(modID[i])) % 1000003
	}
	idx := h % total
	chosen := picked[len(picked)-1]
	for _, st := range picked {
		if idx < st.BodyShare {
			chosen = st
			break
		}
		idx -= st.BodyShare
	}

	ordered := make([]ChainStep, 0, len(cfg.Chain))
	ordered = append(ordered, chosen)
	for _, st := range cfg.Chain {
		if st.Provider == chosen.Provider && st.ModelName == chosen.ModelName {
			continue
		}
		ordered = append(ordered, st)
	}
	return ordered
}

// translateBodyChunked 把正文切块并行翻译；分块前按实际打头 model 的每分钟输出预算做节流
// （预算 0 = 不限速）。分块用哪个模型由 body_share 权重决定，失败会自动走到链上下一个模型。
func translateBodyChunked(cfg *RuntimeConfig, modID, text, lang string, checkBudget bool) (string, error) {
	if len(cfg.Chain) == 0 {
		return "", fmt.Errorf("no chain configured")
	}
	steps := bodyChain(cfg, modID)
	primary := steps[0]

	chunkSize := bodyChunkSize()
	chunks := splitBody(text, chunkSize)
	if len(chunks) == 0 {
		return "", fmt.Errorf("empty body for %s", modID)
	}
	budgetTxt := "unlimited"
	if b := stepOutBudget(primary); b > 0 {
		budgetTxt = fmt.Sprintf("%.0f/min", b)
	}
	log.Printf("body chunked: mod=%s model=%s/%s chars=%d chunk_size=%d chunks=%d tok_budget=%s",
		modID, primary.Provider, primary.ModelName, len(text), chunkSize, len(chunks), budgetTxt)

	buf := make([]string, len(chunks))
	errs := make([]error, len(chunks))

	// 第一轮：并发（但限制在途数）翻译所有分块
	failed := translateChunks(cfg, chunks, lang, steps, checkBudget, buf, errs, nil)

	// 失败分块单独重试：整篇重做会把已成功的块再翻一遍（白烧额度 + 加重限速），
	// 所以只重翻失败的那几块，并且退避一下让账号的分钟级限速恢复。
	for attempt := 1; attempt <= bodyChunkRetryTimes && len(failed) > 0; attempt++ {
		// 被内容审核拒掉的块先剔除：同一段文本换账号也会被同一套审核拦下，重试纯属白烧额度
		retryable := failed[:0]
		for _, idx := range failed {
			if errors.Is(errs[idx], errContentRejected) {
				log.Printf("body chunk %d content rejected, skip retry: mod=%s", idx, modID)
				continue
			}
			retryable = append(retryable, idx)
		}
		failed = retryable
		if len(failed) == 0 {
			break
		}
		wait := time.Duration(attempt) * bodyChunkRetryDelay
		log.Printf("body chunk retry: mod=%s attempt=%d failed=%d wait=%s", modID, attempt, len(failed), wait)
		time.Sleep(wait)
		failed = translateChunks(cfg, chunks, lang, steps, checkBudget, buf, errs, failed)
	}
	if len(failed) > 0 {
		idx := failed[0]
		return "", fmt.Errorf("body chunk %d failed: %v", idx, errs[idx])
	}

	var joined strings.Builder
	for i, t := range buf {
		if i > 0 {
			joined.WriteString("\n\n")
		}
		joined.WriteString(t)
	}
	return joined.String(), nil
}

// translateChunks 并发翻译分块（idxList 传 nil 表示全部），结果写进 buf/errs，
// 返回仍然失败的块下标。在途请求数被限制在 bodyChunkInflight：
// 账号限的是每分钟额度，把 8 块一次性全发出去只会一起撞 429。
func translateChunks(cfg *RuntimeConfig, chunks []string, lang string, steps []ChainStep,
	checkBudget bool, buf []string, errs []error, idxList []int) []int {

	if idxList == nil {
		idxList = make([]int, len(chunks))
		for i := range idxList {
			idxList[i] = i
		}
	}
	if len(idxList) == 0 {
		return nil
	}

	inflight := bodyChunkInflight
	if inflight > len(idxList) {
		inflight = len(idxList)
	}
	if inflight < 1 {
		inflight = 1
	}

	sem := make(chan struct{}, inflight)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := make([]int, 0, len(idxList))

	for _, idx := range idxList {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			// token 预算限速已下沉到 callChainIn，按实际目标 model 施加（含兜底调用）
			out, err := callChainIn(cfg, chunks[i], lang, steps, checkBudget, "body")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[i] = err
				failed = append(failed, i)
				return
			}
			buf[i] = out
		}(idx)
	}
	wg.Wait()
	return failed
}

func fetchModrinthFull(modID string) (string, string, string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * 500 * time.Millisecond
			time.Sleep(backoff)
		}
		desc, body, updated, err := fetchModrinthOnce(modID)
		if err == nil {
			return desc, body, updated, nil
		}
		lastErr = err
		if errors.Is(err, errNotFound) {
			return "", "", "", err
		}
		if errors.Is(err, errRateLimited) {
			return "", "", "", err
		}
	}
	return "", "", "", fmt.Errorf("after 3 attempts: %v", lastErr)
}

// ============== Handlers ==============

func handleHealth(w http.ResponseWriter, r *http.Request) {
	statRequest("health")
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleTranslateMod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	statRequest("translate_mod")
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req TranslateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Platform == "" || req.ModID == "" {
		http.Error(w, "platform and mod_id are required", http.StatusBadRequest)
		return
	}
	if req.Lang == "" {
		req.Lang = "zh"
	}
	if !platformSupported(req.Platform) {
		http.Error(w, "unsupported platform: "+req.Platform+" (supported: modrinth)", http.StatusBadRequest)
		return
	}
	statReqWithBody("translate_mod", req.IncludeBody)

	res := TranslateResponse{}
	text, cached, err := getModTranslation(req.Platform, req.ModID, req.Lang)
	if err != nil {
		log.Printf("translate mod error: %v", err)
		writeJSONStatus(w, http.StatusInternalServerError, TranslateResponse{Error: err.Error()})
		return
	}
	res.Text = text
	res.Cached = cached

	if req.IncludeBody {
		body, bodyCached, berr := getBodyTranslation(req.Platform, req.ModID, req.Lang)
		if berr != nil {
			log.Printf("translate body error: %v", berr)
			res.Error = "body failed: " + berr.Error()
		} else {
			res.Body = body
			res.BodyCached = bodyCached
		}
	}

	writeJSON(w, res)
}

func handleTranslateMods(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	statRequest("translate_mods")
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req BatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Platform == "" {
		http.Error(w, "platform is required", http.StatusBadRequest)
		return
	}
	if !platformSupported(req.Platform) {
		http.Error(w, "unsupported platform: "+req.Platform+" (supported: modrinth)", http.StatusBadRequest)
		return
	}
	if req.Lang == "" {
		req.Lang = "zh"
	}
	if len(req.ModIDs) == 0 {
		http.Error(w, "mod_ids is empty", http.StatusBadRequest)
		return
	}
	if len(req.ModIDs) > maxBatchSize {
		http.Error(w, fmt.Sprintf("too many mod_ids (max %d)", maxBatchSize), http.StatusBadRequest)
		return
	}
	statReqWithBody("translate_mods", req.IncludeBody)

	ip := getClientIP(r)
	statIP(ip)

	lim := translateLimiters.get(ip)
	for i := 0; i < len(req.ModIDs); i++ {
		if !lim.Allow() {
			statRateLimitHit()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limit exceeded"}`))
			log.Printf("rate limit hit (batch): ip=%s count=%d", ip, len(req.ModIDs))
			return
		}
	}

	results := make(map[string]TranslateResponse, len(req.ModIDs))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, modID := range req.ModIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			text, cached, err := getModTranslation(req.Platform, id, req.Lang)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[id] = TranslateResponse{Error: err.Error()}
				return
			}
			res := TranslateResponse{Text: text, Cached: cached}
			if req.IncludeBody {
				body, bodyCached, berr := getBodyTranslation(req.Platform, id, req.Lang)
				if berr != nil {
					res.Error = "body failed: " + berr.Error()
				} else {
					res.Body = body
					res.BodyCached = bodyCached
				}
			}
			results[id] = res
		}(modID)
	}
	wg.Wait()

	log.Printf("translate mods: ip=%s count=%d", ip, len(req.ModIDs))
	writeJSON(w, BatchResponse{Results: results})
}

func handleFeedbackStale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	statRequest("feedback_stale")
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req FeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Platform == "" || req.ModID == "" {
		http.Error(w, "platform and mod_id are required", http.StatusBadRequest)
		return
	}
	if req.Lang == "" {
		req.Lang = "zh"
	}
	if !platformSupported(req.Platform) {
		http.Error(w, "unsupported platform: "+req.Platform+" (supported: modrinth)", http.StatusBadRequest)
		return
	}

	ip := getClientIP(r)
	lim := feedbackLimiters.get(ip)
	if !lim.Allow() {
		log.Printf("feedback limit exceeded (silent): ip=%s mod=%s", ip, req.ModID)
		statStaleFeedback("received")
		writeJSON(w, FeedbackResponse{Status: "received"})
		return
	}

	// 客户端传的标识就是缓存键里的标识（Modrinth 的 slug / nanoid 都不需要归一化）
	id := req.ModID

	cooldownKey := fmt.Sprintf("feedback:cooldown:%s:%s", req.Platform, id)
	if redisAlive {
		if exists, _ := rdb.Exists(ctx, cooldownKey).Result(); exists > 0 {
			statStaleFeedback("cooldown")
			writeJSON(w, FeedbackResponse{Status: "cooldown"})
			return
		}
	}

	cacheKey := modCacheKey(req.Platform, id, req.Lang)
	entry, ok := cacheGetEntry(cacheKey)
	if !ok {
		statStaleFeedback("no_cache")
		writeJSON(w, FeedbackResponse{Status: "no_cache"})
		return
	}

	// 按平台取原站的最新更新时间，和缓存条目里记的做对比
	_, _, latestUpdated, err := fetchModFullByPlatform(req.Platform, id)
	if err != nil {
		log.Printf("feedback: fetch modrinth failed: %v", err)
		statStaleFeedback("error")
		writeJSONStatus(w, http.StatusInternalServerError, FeedbackResponse{Status: "error"})
		return
	}

	if entry.Updated == latestUpdated {
		statStaleFeedback("unchanged")
		writeJSON(w, FeedbackResponse{Status: "unchanged"})
		return
	}

	log.Printf("feedback: mod %s updated from %s to %s, refreshing",
		req.ModID, entry.Updated, latestUpdated)
	cacheDelete(cacheKey)

	if _, err := translateAndCache(req.Platform, id, req.Lang); err != nil {
		log.Printf("feedback: refresh translate failed: %v", err)
		statStaleFeedback("refresh_failed")
		writeJSONStatus(w, http.StatusInternalServerError, FeedbackResponse{Status: "refresh_failed"})
		return
	}

	if redisAlive {
		_ = rdb.Set(ctx, cooldownKey, "1", cooldownDuration).Err()
	}
	statStaleFeedback("updated")
	writeJSON(w, FeedbackResponse{Status: "updated"})
}

func handleFeedbackQuality(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	statRequest("feedback_quality")
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req QualityFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Platform == "" || req.ModID == "" || req.IssueType == "" {
		http.Error(w, "platform, mod_id, and issue_type are required", http.StatusBadRequest)
		return
	}
	if req.Lang == "" {
		req.Lang = "zh"
	}
	if !platformSupported(req.Platform) {
		http.Error(w, "unsupported platform: "+req.Platform+" (supported: modrinth)", http.StatusBadRequest)
		return
	}
	switch req.IssueType {
	case "wrong_translation", "unnatural", "missing", "other":
	default:
		http.Error(w, "invalid issue_type", http.StatusBadRequest)
		return
	}
	if len(req.UserSuggestion) > maxSuggestionLen {
		http.Error(w, fmt.Sprintf("user_suggestion too long (max %d)", maxSuggestionLen), http.StatusBadRequest)
		return
	}
	if len(req.UserComment) > maxSuggestionLen {
		http.Error(w, fmt.Sprintf("user_comment too long (max %d)", maxSuggestionLen), http.StatusBadRequest)
		return
	}

	ip := getClientIP(r)
	lim := qualityLimiters.get(ip)
	if !lim.Allow() {
		log.Printf("quality feedback limit exceeded (silent): ip=%s mod=%s", ip, req.ModID)
		writeJSON(w, FeedbackResponse{Status: "received"})
		return
	}

	_, err := db.Exec(`
		INSERT INTO quality_feedback
			(platform, mod_id, lang, issue_type, user_suggestion, user_comment, ip, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, req.Platform, req.ModID, req.Lang, req.IssueType,
		req.UserSuggestion, req.UserComment, ip, time.Now().Unix())
	if err != nil {
		log.Printf("save quality feedback failed: %v", err)
		writeJSONStatus(w, http.StatusInternalServerError, FeedbackResponse{Status: "error"})
		return
	}
	statQualityFeedback(req.IssueType)
	log.Printf("quality feedback: ip=%s mod=%s issue=%s", ip, req.ModID, req.IssueType)
	writeJSON(w, FeedbackResponse{Status: "received"})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 支持三种用法：?date=2026-09-12（指定某天）、?range=today|yesterday|week|month|all（区间）、默认今天
	date := r.URL.Query().Get("date")
	rng := r.URL.Query().Get("range")
	var res *StatsResult
	var err error
	switch {
	case date != "":
		res, err = getStats(date)
	case rng == "" || rng == "today":
		res, err = getStats(today())
	default:
		res, err = getStatsRange(rng)
	}
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 汇总熔断状态：任何 model 打开则汇总为 open
	res.BreakerStatus = []map[string]interface{}{}
	var anyOpen bool
	breakers.Range(func(k, v interface{}) bool {
		br := v.(*circuitBreaker)
		open, fails, until := br.status()
		if open {
			anyOpen = true
		}
		res.BreakerStatus = append(res.BreakerStatus, map[string]interface{}{
			"key":          k.(string),
			"circuit_open": open,
			"fail_count":   fails,
			"open_until":   until.Unix(),
		})
		return true
	})
	if len(res.BreakerStatus) == 0 {
		anyOpen = false
	}
	res.BreakerOverview = map[string]interface{}{
		"circuit_open": anyOpen,
		"fail_count":   len(res.BreakerStatus),
		"open_until":   0,
	}
	writeJSON(w, res)
}

// ============== 配置 handlers ==============

func handleConfigGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := *getCfg()
	// 必须先把 Providers 复制一份再打码：切片与运行中配置共享底层数组，
	// 直接改会把运行中的真实 key 覆盖成掩码，之后所有上游请求都会鉴权失败（直到重启）。
	providers := make([]Provider, len(cfg.Providers))
	copy(providers, cfg.Providers)
	for i := range providers {
		if providers[i].APIKey != "" {
			providers[i].APIKey = maskedAPIKey
		}
	}
	cfg.Providers = providers
	// 逐接口开关回给面板时带上默认值，保证面板显示的就是实际生效值
	cfg.AuthEndpoints = effectiveAuthEndpoints()
	writeJSON(w, cfg)
}

func handleConfigSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var cfg RuntimeConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// 校验 providers
	if len(cfg.Providers) == 0 {
		http.Error(w, "providers must not be empty", http.StatusBadRequest)
		return
	}
	seen := map[string]bool{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.Name == "" {
			http.Error(w, "provider name required", http.StatusBadRequest)
			return
		}
		if seen[p.Name] {
			http.Error(w, "duplicate provider name: "+p.Name, http.StatusBadRequest)
			return
		}
		seen[p.Name] = true
		if p.Type != "openai" && p.Type != "gemini" {
			http.Error(w, "provider type must be openai or gemini", http.StatusBadRequest)
			return
		}
		if p.BaseURL == "" {
			http.Error(w, "provider base_url required: "+p.Name, http.StatusBadRequest)
			return
		}
		if p.Concurrent < 1 || p.Concurrent > 100 {
			http.Error(w, "provider concurrent must be 1-100", http.StatusBadRequest)
			return
		}
		if p.TimeoutSec < 1 || p.TimeoutSec > 120 {
			http.Error(w, "provider timeout_sec must be 1-120", http.StatusBadRequest)
			return
		}
	}
	// 掩码 key 保留已保存的真实值
	resolveMaskedKeys(&cfg)
	// PrevName 只用于本次请求里找回 key，不落库
	for i := range cfg.Providers {
		cfg.Providers[i].PrevName = ""
	}

	// 校验 chain
	for _, st := range cfg.Chain {
		if !seen[st.Provider] {
			http.Error(w, "chain references unknown provider: "+st.Provider, http.StatusBadRequest)
			return
		}
		if st.ModelName == "" {
			http.Error(w, "chain model required", http.StatusBadRequest)
			return
		}
		if st.DailyLimit < 0 {
			http.Error(w, "chain daily_limit must be >= 0", http.StatusBadRequest)
			return
		}
		if st.BodyShare < 0 {
			http.Error(w, "chain body_share must be >= 0", http.StatusBadRequest)
			return
		}
		if st.RPMPerMin < 0 || st.RPMPerMin > 100000 {
			http.Error(w, "chain rpm must be 0-100000", http.StatusBadRequest)
			return
		}
		if st.CooldownSec < 0 || st.CooldownSec > 3600 {
			http.Error(w, "chain rate_limit_cooldown_sec must be 0-3600", http.StatusBadRequest)
			return
		}
	}
	// AI 管家：选了 provider 就必须给出模型名，且该 provider 必须存在
	if cfg.AdvisorProvider != "" {
		if !seen[cfg.AdvisorProvider] {
			http.Error(w, "advisor provider not found: "+cfg.AdvisorProvider, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(cfg.AdvisorModel) == "" {
			http.Error(w, "advisor model required when advisor_provider is set", http.StatusBadRequest)
			return
		}
	}
	if cfg.BodyChunkChars != 0 && (cfg.BodyChunkChars < minBodyChunkChars || cfg.BodyChunkChars > maxBodyChunkChars) {
		http.Error(w, fmt.Sprintf("body_chunk_chars must be between %d and %d", minBodyChunkChars, maxBodyChunkChars), http.StatusBadRequest)
		return
	}
	if cfg.BreakerThreshold <= 0 {
		http.Error(w, "breaker_threshold must be > 0", http.StatusBadRequest)
		return
	}
	if cfg.BreakerDurationMin <= 0 {
		http.Error(w, "breaker_duration_min must be > 0", http.StatusBadRequest)
		return
	}

	if err := saveRuntimeConfig(&cfg); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	applyRuntimeConfig(&cfg)
	log.Printf("config updated via API")
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleConfigReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := defaultConfig()
	if redisAlive {
		rdb.Del(ctx, configRedisKey)
	}
	applyRuntimeConfig(cfg)
	log.Printf("config reset to defaults")
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleConfigTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Provider string         `json:"provider"` // 指定 provider；空则测整条链
		Model    string         `json:"model"`
		Config   *RuntimeConfig `json:"config"` // 可选：面板里尚未保存的配置，带上就按它测试
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	testText := "Sodium is a high-performance rendering engine for Minecraft."
	cfg := getCfg()
	if req.Config != nil {
		if len(req.Config.Providers) == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "providers must not be empty"})
			return
		}
		// 掩码/空 key 还原成已保存的真实值：只改了一部分字段也能整体测
		resolveMaskedKeys(req.Config)
		cfg = req.Config
	}

	// 测试整条链
	if req.Provider == "" {
		start := time.Now()
		text, err := callChainIn(cfg, testText, "zh", cfg.Chain, false, "test")
		elapsed := time.Since(start)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "elapsed": elapsed.Milliseconds()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "elapsed": elapsed.Milliseconds(), "result": truncateForTest(text)})
		return
	}

	// 测试指定 provider（单模型或该 provider 首个模型）
	p := findProviderIn(cfg, req.Provider)
	if p == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "unknown provider: " + req.Provider})
		return
	}
	model := req.Model
	if model == "" {
		// 取该 provider 在链上的第一个模型
		for _, st := range cfg.Chain {
			if st.Provider == p.Name {
				model = st.ModelName
				break
			}
		}
	}
	if model == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "no model for provider " + p.Name})
		return
	}
	start := time.Now()
	text, err := callModel(*p, model, testText, "zh")
	elapsed := time.Since(start)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "elapsed": elapsed.Milliseconds()})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "elapsed": elapsed.Milliseconds(), "model": p.Name + "/" + model, "result": truncateForTest(text)})
}

func truncateForTest(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// ============== 缓存浏览（只读数据库，不触发翻译） ==============

// parseCacheKey 解析缓存键：
// 描述 trans:mod:<platform>:<modID>:<lang>，正文 trans:mod:body:<platform>:<modID>:<lang>
func parseCacheKey(key string) (kind, platform, modID, lang string) {
	parts := strings.Split(key, ":")
	if len(parts) < 5 || parts[0] != "trans" || parts[1] != "mod" {
		return "", "", "", ""
	}
	idx := 2
	kind = "desc"
	if parts[2] == "body" {
		kind = "body"
		idx = 3
	}
	if len(parts) < idx+3 {
		return kind, "", "", ""
	}
	return kind, parts[idx], parts[idx+1], parts[idx+2]
}

// truncateRunes 按字符（而非字节）截断，避免把中文切坏。
func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// handleCacheList 关键词搜索 + 分页列出缓存条目。
func handleCacheList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "all"
	}
	lang := strings.TrimSpace(r.URL.Query().Get("lang"))
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	limit := 30
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}

	where := []string{"key LIKE 'trans:mod:%'"}
	args := []interface{}{}
	if q != "" {
		where = append(where, "key LIKE ?")
		args = append(args, "%"+q+"%")
	}
	if lang != "" {
		where = append(where, "key LIKE ?")
		args = append(args, "%:"+lang)
	}
	if platform != "" && platform != "all" {
		// 描述与正文的 key 形状不同（trans:mod:<平台>:… / trans:mod:body:<平台>:…），所以用两条 LIKE
		where = append(where, "(key LIKE ? OR key LIKE ?)")
		args = append(args, "trans:mod:"+platform+":%", "trans:mod:body:"+platform+":%")
	}
	base := strings.Join(where, " AND ")

	var total, descCount, bodyCount int64
	_ = db.QueryRow("SELECT COUNT(*) FROM translations WHERE "+base, args...).Scan(&total)
	_ = db.QueryRow("SELECT COUNT(*) FROM translations WHERE "+base+" AND key NOT LIKE 'trans:mod:body:%'", args...).Scan(&descCount)
	_ = db.QueryRow("SELECT COUNT(*) FROM translations WHERE "+base+" AND key LIKE 'trans:mod:body:%'", args...).Scan(&bodyCount)

	// 分来源计数：面板的来源筛选与计数都用它（加内容源时这里跟着 supportedPlatforms 走）
	platformCounts := map[string]int64{}
	for _, p := range supportedPlatforms() {
		var c int64
		_ = db.QueryRow("SELECT COUNT(*) FROM translations WHERE "+base+" AND (key LIKE ? OR key LIKE ?)",
			append(append([]interface{}{}, args...), "trans:mod:"+p+":%", "trans:mod:body:"+p+":%")...).Scan(&c)
		platformCounts[p] = c
	}

	filter := base
	if kind == "body" {
		filter += " AND key LIKE 'trans:mod:body:%'"
	} else if kind == "desc" {
		filter += " AND key NOT LIKE 'trans:mod:body:%'"
	}

	items := []map[string]interface{}{}
	rows, err := db.Query("SELECT key, text, updated_at FROM translations WHERE "+filter+" ORDER BY updated_at DESC LIMIT ? OFFSET ?",
		append(append([]interface{}{}, args...), limit, offset)...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key, raw string
			var updatedAt int64
			if err := rows.Scan(&key, &raw, &updatedAt); err != nil {
				continue
			}
			// 缓存值存的是 CacheEntry 的 JSON；解不出来就按纯文本处理
			entry := CacheEntry{Text: raw}
			_ = json.Unmarshal([]byte(raw), &entry)
			k, platform, modID, lg := parseCacheKey(key)
			items = append(items, map[string]interface{}{
				"key":           key,
				"kind":          k,
				"platform":      platform,
				"mod_id":        modID,
				"lang":          lg,
				"chars":         len([]rune(entry.Text)),
				"updated":       entry.Updated,
				"translated_at": entry.TranslatedAt,
				"updated_at":    updatedAt,
				"preview":       truncateRunes(entry.Text, 160),
			})
		}
	}
	writeJSON(w, map[string]interface{}{
		"total":           total,
		"desc_count":      descCount,
		"body_count":      bodyCount,
		"platform":        platform,
		"platform_counts": platformCounts,
		"offset":          offset,
		"limit":           limit,
		"items":           items,
		"server_time":     time.Now().Unix(),
	})
}

// handleCacheGet 返回单条缓存的全文。
func handleCacheGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "key required"})
		return
	}
	var raw string
	var updatedAt int64
	if err := db.QueryRow("SELECT text, updated_at FROM translations WHERE key = ?", key).Scan(&raw, &updatedAt); err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	entry := CacheEntry{Text: raw}
	_ = json.Unmarshal([]byte(raw), &entry)
	k, platform, modID, lg := parseCacheKey(key)

	// 顺手把同一个 mod 的另一半带出来（点正文能同时看到描述，反之亦然），前端一次请求即可对照
	siblingKey := modCacheKey(platform, modID, lg)
	if k == "desc" {
		siblingKey = modBodyCacheKey(platform, modID, lg)
	}
	sibling := map[string]interface{}{}
	var sraw string
	var supdated int64
	if err := db.QueryRow("SELECT text, updated_at FROM translations WHERE key = ?", siblingKey).Scan(&sraw, &supdated); err == nil {
		sentry := CacheEntry{Text: sraw}
		_ = json.Unmarshal([]byte(sraw), &sentry)
		sk, _, _, _ := parseCacheKey(siblingKey)
		sibling = map[string]interface{}{
			"key":           siblingKey,
			"kind":          sk,
			"chars":         len([]rune(sentry.Text)),
			"translated_at": sentry.TranslatedAt,
			"updated_at":    supdated,
			"text":          sentry.Text,
		}
	}

	writeJSON(w, map[string]interface{}{
		"key":           key,
		"kind":          k,
		"platform":      platform,
		"mod_id":        modID,
		"lang":          lg,
		"chars":         len([]rune(entry.Text)),
		"updated":       entry.Updated,
		"translated_at": entry.TranslatedAt,
		"updated_at":    updatedAt,
		"text":          entry.Text,
		"sibling":       sibling,
	})
}

// ============== 运行日志（面板"按模型查看日志"用） ==============

// defaultLogPath 是 systemd 重定向写出的主日志路径，可用 LOG_PATH 覆盖。
const defaultLogPath = "./trans.log"

func logFilePath() string { return getEnv("LOG_PATH", defaultLogPath) }

// handleWorkerLogs 读取日志尾部，并按 model / 关键词过滤。
// 面板"运行日志"页给每个模型单独的查看位置，就是靠这里的 model 参数。
func handleWorkerLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lines := 400
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 && v <= 5000 {
		lines = v
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	kw := strings.TrimSpace(r.URL.Query().Get("q"))

	path := logFilePath()
	f, err := os.Open(path)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "open log failed: " + err.Error()})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "stat log failed: " + err.Error()})
		return
	}

	// 只读末尾 1MB：日志可能很大，没必要整份读进内存
	const window = 1 << 20
	size := st.Size()
	start := int64(0)
	if size > window {
		start = size - window
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "read log failed: " + err.Error()})
		return
	}
	all := strings.Split(string(buf), "\n")
	if start > 0 && len(all) > 0 {
		all = all[1:] // 丢掉可能被截断的首行
	}

	matched := make([]string, 0, lines)
	total, hit := 0, 0
	for _, ln := range all {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		total++
		if model != "" && !strings.Contains(ln, model) {
			continue
		}
		if kw != "" && !strings.Contains(ln, kw) {
			continue
		}
		hit++
		matched = append(matched, ln)
		if len(matched) > lines {
			matched = matched[1:] // 只保留最后 lines 行
		}
	}
	writeJSON(w, map[string]interface{}{
		"lines":       matched,
		"returned":    len(matched),
		"matched":     hit,
		"total_lines": total,
		"model":       model,
		"q":           kw,
		"file":        path,
	})
}

// ============== Modrinth ==============

var errNotFound = errors.New("upstream 404 not found")
var errRateLimited = errors.New("upstream rate limited")

func fetchModrinthOnce(modID string) (string, string, string, error) {
	if !modrinthLimiter.Allow() {
		return "", "", "", errRateLimited
	}
	statModrinthCall()

	url := "https://api.modrinth.com/v2/project/" + modID
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "trans-cache/1.0")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		statModrinth404()
		return "", "", "", errNotFound
	}
	if resp.StatusCode == 429 {
		statModrinth429()
		return "", "", "", errRateLimited
	}
	if resp.StatusCode != 200 {
		return "", "", "", fmt.Errorf("modrinth returned %d", resp.StatusCode)
	}

	var proj ModrinthProject
	if err := json.NewDecoder(resp.Body).Decode(&proj); err != nil {
		return "", "", "", err
	}
	return proj.Description, proj.Body, proj.Updated, nil
}

// ============== LLM（多 Provider / 多 Model 统一调用链） ==============

const llmSystemPrompt = "你是一个 Minecraft Mod 描述翻译器。请把用户输入的英文 Mod 描述翻译成简体中文，保持术语准确、语句通顺。只输出翻译结果，不要解释，不要思考。"

// callLLM 客户端翻译入口：走整条链，不做预算限制。
func callLLM(text, lang string) (string, error) {
	return callChain(text, lang, getCfg().Chain, false)
}

// callChainForWorker Worker 入口：走整条链，并逐 model 校验每日预算。
func callChainForWorker(text, lang string) (string, error) {
	return callChain(text, lang, getCfg().Chain, true)
}

// callChain 沿当前生效配置的 chain 逐 model 尝试，第一个成功即返回。
func callChain(text, lang string, chain []ChainStep, checkBudget bool) (string, error) {
	return callChainIn(getCfg(), text, lang, chain, checkBudget, "other")
}

// ============== 限速冷却（与熔断解耦） ==============

// 限速（429 / 账户速率限制）是分钟级就能恢复的临时错误，跟 500、鉴权失败这类硬失败性质不同：
// 按硬失败计熔断的话，阈值 3 次就会把该 provider 锁死几分钟，负载被推给下一个 provider，
// 很快全员熔断 —— 实测这条链会把同一类型下的多个账号一起拖垮、正文批量失败。
// 所以限速只做「短暂冷却」：冷却期内直接跳过这个 provider（几乎零成本），不计入熔断。
const rateLimitCooldownDur = 15 * time.Second

var rateLimitCooldownUntil sync.Map // key: "provider/model" -> time.Time

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{"速率限制", "rate limited", "429", "Too Many Requests", "RPM", "TPM", "OTPM"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func setRateLimitCooldown(key string) {
	setRateLimitCooldownFor(key, rateLimitCooldownDur)
}

// setRateLimitCooldownFor 按指定时长冷却：有些上游限速后要等更久（例如强制一分钟），
// 用默认 15s 重试等于再撞一次墙，所以冷却时长支持按模型覆盖。
func setRateLimitCooldownFor(key string, d time.Duration) {
	rateLimitCooldownUntil.Store(key, time.Now().Add(d))
}

func clearRateLimitCooldown(key string) {
	rateLimitCooldownUntil.Delete(key)
}

// rateLimitCooling 返回剩余冷却时间，0 表示不在冷却中。
func rateLimitCooling(key string) time.Duration {
	if v, ok := rateLimitCooldownUntil.Load(key); ok {
		if t, ok := v.(time.Time); ok {
			if d := time.Until(t); d > 0 {
				return d
			}
			rateLimitCooldownUntil.Delete(key)
		}
	}
	return 0
}

// callChainIn 用指定配置沿 chain 逐 model 尝试，第一个成功即返回。
// 每个步骤失败/被跳过的原因都收集进错误信息：只留最后一个错误会把前面 provider 的真实原因盖掉。
// kind 只用于面板的实时槽位视图（desc / body / test / other），不影响调用行为。
func callChainIn(cfg *RuntimeConfig, text, lang string, chain []ChainStep, checkBudget bool, kind string) (string, error) {
	var errs []string
	attempted, contentRejected := 0, 0 // 真正打到上游的次数 / 其中被内容审核拒掉的次数
	tok := int(float64(len(text)) * outTokFactor)
	if tok < 1 {
		tok = 1
	}
	for _, st := range chain {
		p := findProviderIn(cfg, st.Provider)
		if p == nil {
			errs = append(errs, st.Provider+": provider not found")
			continue
		}
		key := p.Name + "/" + st.ModelName
		br := getModelBreaker(p.Name, st.ModelName)
		if br.isBroken() {
			log.Printf("[BREAKER] skip %s (circuit open)", key)
			errs = append(errs, key+": circuit open")
			continue
		}
		if left := rateLimitCooling(key); left > 0 {
			log.Printf("[COOLDOWN] skip %s (限速冷却中，还剩 %.0fs)", key, left.Seconds())
			errs = append(errs, key+": cooling down after rate limit")
			continue
		}
		if checkBudget && st.DailyLimit > 0 && modelTodayUsed(p.Name, st.ModelName) >= st.DailyLimit {
			log.Printf("[WORKER] skip %s: daily limit reached (%d/%d)", key, modelTodayUsed(p.Name, st.ModelName), st.DailyLimit)
			errs = append(errs, key+": daily limit reached")
			continue
		}
		// 按目标 model 自己的每分钟 token 预算排队：兜底调用同样受限，避免失败回落把某个 model 打爆
		if stepOutBudget(st) > 0 {
			waitCtx, cancel := context.WithTimeout(context.Background(), tokWaitTimeout)
			werr := tokWait(waitCtx, modelTokLimiter(st), tok)
			cancel()
			if werr != nil {
				errs = append(errs, key+": token budget wait failed")
				continue
			}
		}
		// 按目标 model 自己的 RPM 上限排队（请求间隔 60/RPM 秒）。
		// 免费档常硬限 RPM，撞上去会被强制冷却一分钟 —— 排几秒队远比吃一分钟冷却划算。
		if stepRPM(st) > 0 {
			waitID := liveWaitStart(p.Name, st.ModelName, "rpm")
			rpmStart := time.Now()
			waitCtx, cancel := context.WithTimeout(context.Background(), rpmWaitTimeout)
			werr := modelRPMLimiter(st).Wait(waitCtx)
			cancel()
			liveWaitEnd(p.Name, waitID)
			if werr != nil {
				log.Printf("[RPM] %s 排队超过 %s，换下一个模型", key, rpmWaitTimeout)
				errs = append(errs, fmt.Sprintf("%s: rpm wait > %s", key, rpmWaitTimeout))
				continue
			}
			if waited := time.Since(rpmStart); waited > 2*time.Second {
				log.Printf("[RPM] %s 排队 %.1fs 才拿到名额（上限 %.0f 次/分）", key, waited.Seconds(), stepRPM(st))
			}
		}
		// 不在信号量表里的 provider（例如测试尚未保存的新 provider）不限并发，而不是静默跳过该 model
		semWaitStart := time.Now()
		sem := getProviderSem(p.Name)
		if sem != nil {
			// 等槽位要有上限：等不到就直接走下一个 model，别把 worker 槽位耗在排队上
			waitID := liveWaitStart(p.Name, st.ModelName, "slot")
			ok := acquireProviderSem(sem, providerSemWaitTimeout)
			liveWaitEnd(p.Name, waitID)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: provider slot busy > %s", key, providerSemWaitTimeout))
				continue
			}
		}
		semWait := time.Since(semWaitStart)
		callID := liveCallStart(p.Name, st.ModelName, kind)
		callStart := time.Now()
		result, err := callModel(*p, st.ModelName, text, lang)
		callDur := time.Since(callStart)
		if sem != nil {
			<-sem
		}
		liveCallEnd(p.Name, callID)
		if semWait > 3*time.Second || callDur > 8*time.Second {
			log.Printf("[LLM] %s semwait=%.1fs call=%.1fs err=%v", key, semWait.Seconds(), callDur.Seconds(), err != nil)
		}
		if err == nil {
			br.recordSuccess()
			clearRateLimitCooldown(key)
			return result, nil
		}
		if isRateLimitError(err) {
			// 限速不计入熔断，只冷却一段时间让后续请求绕开它（见 rateLimitCooldownDur 的注释）。
			// 冷却时长可按模型覆盖：有的上游超限即罚一分钟，15 秒后重试纯属再撞一次。
			d := cooldownFor(st)
			setRateLimitCooldownFor(key, d)
			log.Printf("[RATE-LIMIT] %s 触发限速，冷却 %s（不计入熔断）", key, d)
		} else {
			br.recordFailure()
		}
		log.Printf("model %s failed: %v", key, err)
		attempted++
		if isContentRejectError(err) {
			contentRejected++
		}
		errs = append(errs, key+": "+err.Error())
	}
	if len(errs) == 0 {
		return "", fmt.Errorf("chain is empty")
	}
	// 所有真正打到上游的请求都被内容审核拒了：包成哨兵错误，让上层不要重试、也不要排进回访队列
	if attempted > 0 && contentRejected == attempted {
		return "", fmt.Errorf("%w: %s", errContentRejected, strings.Join(errs, " | "))
	}
	return "", fmt.Errorf("all models failed: %s", strings.Join(errs, " | "))
}

// acquireProviderSem 在 timeout 内获取 provider 并发槽；拿不到返回 false（调用方换下一个 model）。
func acquireProviderSem(sem chan struct{}, timeout time.Duration) bool {
	if timeout <= 0 {
		sem <- struct{}{}
		return true
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case sem <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

// callModel 按 provider type 分发到对应 HTTP 实现。
func callModel(p Provider, model, text, lang string) (string, error) {
	switch p.Type {
	case "openai":
		return callOpenAI(p, model, text, lang)
	case "gemini":
		return callGemini(p, model, text, lang)
	default:
		return "", fmt.Errorf("unknown provider type: %s", p.Type)
	}
}

// openAIChatURL 归一化 OpenAI 兼容接口地址，保证以 /chat/completions 结尾。
func openAIChatURL(baseURL string) string {
	b := strings.TrimSuffix(baseURL, "/")
	if strings.HasSuffix(b, "/chat/completions") {
		return b
	}
	if strings.HasSuffix(b, "/v1") {
		return b + "/chat/completions"
	}
	return b + "/v1/chat/completions"
}

// callOpenAI 通用 OpenAI 兼容 /chat/completions 实现。
func callOpenAI(p Provider, model, text, lang string) (string, error) {
	start := time.Now()
	result, err := callOpenAIImpl(p, model, text, lang)
	statLLMTrace(p.Type, p.Name, model, err == nil, time.Since(start).Milliseconds())
	return result, err
}

func callOpenAIImpl(p Provider, model, text, lang string) (string, error) {
	chatReq := ChatRequest{
		Model: model,
		Messages: []ChatMessage{
			{Role: "system", Content: llmSystemPrompt},
			{Role: "user", Content: text},
		},
		Stream: false,
	}
	body, _ := json.Marshal(chatReq)

	httpReq, err := http.NewRequest("POST", openAIChatURL(p.BaseURL), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := &http.Client{Timeout: time.Duration(p.TimeoutSec) * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("%s unmarshal failed: %v, body: %s", p.Name, err, string(respBody))
	}
	if chatResp.Error != nil {
		return "", fmt.Errorf("%s error: %s", p.Name, chatResp.Error.Message)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("%s empty choices, body: %s", p.Name, string(respBody))
	}
	return chatResp.Choices[0].Message.Content, nil
}

// callGemini 通用 gemini generateContent 实现。
func callGemini(p Provider, model, text, lang string) (string, error) {
	start := time.Now()
	result, err := callGeminiImpl(p, model, text, lang)
	statLLMTrace(p.Type, p.Name, model, err == nil, time.Since(start).Milliseconds())
	return result, err
}

func callGeminiImpl(p Provider, model, text, lang string) (string, error) {
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s",
		strings.TrimSuffix(p.BaseURL, "/"), model, p.APIKey)

	reqBody := map[string]interface{}{
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]string{
				{"text": llmSystemPrompt},
			},
		},
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]string{
					{"text": text},
				},
			},
		},
	}
	body, _ := json.Marshal(reqBody)

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: time.Duration(p.TimeoutSec) * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 429 {
		return "", fmt.Errorf("%s rate limited (429)", p.Name)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s returned %d: %s", p.Name, resp.StatusCode, string(respBody))
	}

	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return "", fmt.Errorf("%s unmarshal failed: %v, body: %s", p.Name, err, string(respBody))
	}
	if geminiResp.Error != nil {
		return "", fmt.Errorf("%s error: %s", p.Name, geminiResp.Error.Message)
	}
	if len(geminiResp.Candidates) == 0 ||
		len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("%s empty response, body: %s", p.Name, string(respBody))
	}
	return geminiResp.Candidates[0].Content.Parts[0].Text, nil
}

// ============== util ==============

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	v2, _ := json.Marshal(v)
	w.Write(v2)
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
