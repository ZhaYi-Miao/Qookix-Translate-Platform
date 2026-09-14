package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ============== 对外只读状态（文档站 / 状态页用） ==============
//
// 文档站与 trans 不同源，浏览器直接拉 /worker/status 会被 CORS 拦掉，而且那个接口
// 含少量内部字段。这里提供一个专门对外的聚合视图：
//   GET /public/status
// 只暴露"给外人看"的字段：在线状态、worker 阶段/进度/速率、今日翻译量、各模型成功率。
// 绝不含：IP、mod 名、配置、密钥、缓存明细。
// 带 15 秒服务端缓存（状态页轮询再密也只打这么多次）与 CORS 头（默认 *，
// 可用环境变量 STATUS_CORS_ORIGINS=https://你的文档站 收窄）。

const publicStatusTTL = 15 * time.Second

var (
	publicStatusMu     sync.Mutex
	publicStatusAt     time.Time
	publicStatusCached []byte
)

// handlePublicStatus 组装对外状态快照。
func handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", statusCORSOrigin(r))
	w.Header().Set("Cache-Control", "public, max-age=10")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	publicStatusMu.Lock()
	if publicStatusCached != nil && time.Since(publicStatusAt) < publicStatusTTL {
		body := publicStatusCached
		publicStatusMu.Unlock()
		_, _ = w.Write(body)
		return
	}
	publicStatusMu.Unlock()

	payload := buildPublicStatus()
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	publicStatusMu.Lock()
	publicStatusCached = body
	publicStatusAt = time.Now()
	publicStatusMu.Unlock()
	_, _ = w.Write(body)
}

// statusCORSOrigin 默认放行所有来源；可用 STATUS_CORS_ORIGINS（逗号分隔）收窄。
func statusCORSOrigin(r *http.Request) string {
	allow := getEnv("STATUS_CORS_ORIGINS", "*")
	if allow == "*" {
		return "*"
	}
	origin := r.Header.Get("Origin")
	for _, o := range splitComma(allow) {
		if o == origin {
			return origin
		}
	}
	return splitComma(allow)[0]
}

func buildPublicStatus() map[string]interface{} {
	runningNow := false
	workerMu.Lock()
	runningNow = workerRunning
	workerMu.Unlock()

	tp := throughputSnapshot()
	todayOK, todayFail := getTodayStats()

	// 对外只给「是否在跑」：阶段、进度、失败数都是内部细节，不公开
	// （状态页只显示"后台翻译缓存运行中"，不给用户看 78/439 这种进度）
	worker := map[string]interface{}{"running": runningNow}
	speed := map[string]interface{}{
		"ready":         tp["ready"],
		"chars_per_min": tp["chars_per_min"],
		"mods_per_min":  tp["mods_per_min"],
		"window_sec":    tp["window_sec"],
	}
	// 总体状态只用于给状态页横幅上色（ok / degraded），
	// 不再回传任何带数字的说明文案：模型成功率、限速、失败重试这些都属于内部信息。
	totalCalls, totalFailed := dayCallStats(today())
	state := "operational"
	if totalCalls >= 20 {
		if rate := float64(totalCalls-totalFailed) / float64(totalCalls); rate < 0.95 {
			state = "degraded"
		}
	}

	return map[string]interface{}{
		"online":      true,
		"state":       state,
		"server_time": time.Now().Unix(),
		"worker":      worker,
		"speed":       speed,
		"today": map[string]interface{}{
			"chars": tp["chars_total"],
			"mods":  todayOK,
			"fail":  todayFail,
			// calls / body_ok 是"今天真在干活"的证据：
			//   calls   = 今日模型调用次数（每翻译一个分块 +1，涨得最快，最像"在干活"）
			//   body_ok = 今日补成功的正文篇数（补正文不计入 mods，所以 mods 会长时间不动）
			"calls":   totalCalls,
			"body_ok": getTodayBodyOk(),
		},
	}
}

func okRate(calls, failed int64) float64 {
	if calls <= 0 {
		return 0
	}
	rate := float64(calls-failed) / float64(calls)
	if rate < 0 {
		rate = 0
	}
	return float64(int(rate*1000)) / 1000
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if c != ' ' {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	if len(out) == 0 {
		out = append(out, "*")
	}
	return out
}
