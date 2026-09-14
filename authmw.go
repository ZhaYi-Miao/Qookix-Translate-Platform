package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ============== 接口鉴权（面板可按组开关） ==============
//
// API 的鉴权原本全在 nginx（auth_basic）。为了让面板能按组开关，把校验搬进 Go：
// nginx 只保留面板静态页 / logo.png / logs.txt 的固定鉴权；/stats、/worker/*、/config/*
// 由这里的中间件按 RuntimeConfig 的开关决定是否要求 Basic 认证。
//
// 密码文件沿用 nginx 的 htpasswd（$apr1$ = Apache MD5 crypt），不引入新依赖。
// 慢哈希校验不便宜，这里把验证通过的凭据缓存 15 分钟。
// 读不到密码文件时 fail-closed：管理接口全部拒绝，宁可锁死也不裸奔。

const defaultHtpasswdPath = "./trans.htpasswd"

var (
	htMu       sync.Mutex
	htLoadedAt time.Time
	htEntries  [][2]string // [user, hash]

	authCacheMu sync.Mutex
	authCache   = map[string]time.Time{} // sha256(Authorization 头) -> 最近一次验证通过时间
)

func htpasswdPath() string { return getEnv("HTPASSWD_PATH", defaultHtpasswdPath) }

// loadHtpasswd 每 60s 重读一次文件（改了 htpasswd 后最迟 1 分钟生效）。
func loadHtpasswd(force bool) [][2]string {
	htMu.Lock()
	defer htMu.Unlock()
	if !force && htEntries != nil && time.Since(htLoadedAt) < time.Minute {
		return htEntries
	}
	data, err := os.ReadFile(htpasswdPath())
	if err != nil {
		log.Printf("[AUTH] 读取 %s 失败: %v（管理接口将拒绝访问，fail-closed）", htpasswdPath(), err)
		if htEntries == nil {
			htLoadedAt = time.Now()
		}
		return htEntries
	}
	var out [][2]string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, ":"); i > 0 {
			out = append(out, [2]string{line[:i], line[i+1:]})
		}
	}
	htEntries = out
	htLoadedAt = time.Now()
	return htEntries
}

// checkBasicAuth 校验请求携带的 Basic 凭据；验证通过的凭据缓存 15 分钟。
func checkBasicAuth(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Basic ") {
		return false
	}
	sum := sha256.Sum256([]byte(h))
	key := hex.EncodeToString(sum[:])

	authCacheMu.Lock()
	if t, ok := authCache[key]; ok && time.Since(t) < 15*time.Minute {
		authCacheMu.Unlock()
		return true
	}
	authCacheMu.Unlock()

	ok := verifyHtpasswd(r)
	if ok {
		authCacheMu.Lock()
		if len(authCache) > 1000 {
			authCache = map[string]time.Time{}
		}
		authCache[key] = time.Now()
		authCacheMu.Unlock()
	}
	return ok
}

func verifyHtpasswd(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	for _, e := range loadHtpasswd(false) {
		if e[0] == user && apr1Verify(pass, e[1]) {
			return true
		}
	}
	return false
}

// ---- 逐接口开关 ----
//
// 每个接口是否需要 Basic 认证由 RuntimeConfig.AuthEndpoints（path -> 是否需要）决定；
// 未显式配置的走默认表：管理接口默认要鉴权，客户端接口默认公开。
// 静态页 / logo.png / logs.txt 由 nginx 固定鉴权，不在此列（Go 管不到静态文件）。

// allAPIPaths 全部接口（顺序即面板展示顺序）。
var allAPIPaths = []string{
	// 客户端接口（模组 / 文档站状态页用）：默认公开
	"/health",
	"/public/status",
	"/public/uptime",
	"/translate/mod",
	"/translate/mods",
	"/feedback/stale",
	"/feedback/quality",
	// 管理接口：默认需要鉴权
	// 事件明细含每天的调用/失败数量，只给后台看，不对公众开放
	"/public/incidents",
	"/stats",
	"/worker/status",
	"/worker/start",
	"/worker/stop",
	"/worker/toggle",
	"/worker/preheat/cursor",
	"/worker/logs",
	"/worker/cache/list",
	"/worker/cache/get",
	"/advisor/snapshot",
	"/advisor/ask",
	"/advisor/last",
	"/advisor/apply",
	"/advisor/rollback",
	"/live",
	"/worker/feedback/list",
	"/worker/feedback/status",
	"/worker/feedback/retranslate",
	"/config/get",
	"/config/set",
	"/config/reset",
	"/config/test",
}

// publicByDefault 默认公开的接口；其余接口（含将来新增的）默认需要鉴权，避免漏配裸奔。
var publicByDefault = map[string]bool{
	"/health":           true,
	"/public/status":    true,
	"/public/uptime":    true,
	"/translate/mod":    true,
	"/translate/mods":   true,
	"/feedback/stale":   true,
	"/feedback/quality": true,
}

func defaultAuth(path string) bool { return !publicByDefault[path] }

// forceAuth 无论面板怎么配都强制鉴权的接口。用于"曾经是公开、后来收紧"的接口：
// 面板保存配置时会把当时生效的开关值写进 RuntimeConfig，如果只改默认表，
// 老配置里存的"免鉴权"仍然会生效 —— 所以这类接口必须在这里硬锁。
// 含每天的调用/失败数量这类内部数据，不允许对外开放。
var forceAuth = map[string]bool{
	"/public/incidents": true,
}

// authRequired 查询某接口是否需要鉴权：强制表 > 配置 > 默认表。
func authRequired(path string) bool {
	if forceAuth[path] {
		return true
	}
	if m := getCfg().AuthEndpoints; m != nil {
		if v, ok := m[path]; ok {
			return v
		}
	}
	return defaultAuth(path)
}

// effectiveAuthEndpoints 合并默认值后的完整开关表，供 /config/get 回给面板。
func effectiveAuthEndpoints() map[string]bool {
	out := make(map[string]bool, len(allAPIPaths))
	for _, p := range allAPIPaths {
		out[p] = authRequired(p)
	}
	return out
}

// guardAPI 按接口逐个判断是否需要鉴权。
func guardAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authRequired(r.URL.Path) && !checkBasicAuth(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "401 unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ---- $apr1$（Apache MD5 crypt）校验 ----

const apr1Alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func apr1To64(v uint, n int) string {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = apr1Alphabet[v&0x3f]
		v >>= 6
	}
	return string(out)
}

// apr1Verify 校验形如 $apr1$salt$digest 的哈希；实现有误时只会拒绝（fail-closed），不会放行。
func apr1Verify(password, hash string) bool {
	parts := strings.SplitN(hash, "$", 4)
	if len(parts) != 4 || parts[1] != "apr1" || len(parts[3]) != 22 {
		return false
	}
	return apr1Crypt(password, parts[2]) == parts[3]
}

// apr1Crypt 计算 Apache $apr1$ 哈希的 digest 部分（与 openssl passwd -apr1 / htpasswd 兼容）。
func apr1Crypt(pw, salt string) string {
	if len(salt) > 8 {
		salt = salt[:8]
	}

	// 摘要 B：md5(pw + salt + pw)
	hb := md5.New()
	hb.Write([]byte(pw))
	hb.Write([]byte(salt))
	hb.Write([]byte(pw))
	db := hb.Sum(nil)

	// 摘要 A：流式上下文，pwd + "$apr1$" + salt 起步，后续追加全部写进同一上下文
	ha := md5.New()
	ha.Write([]byte(pw))
	ha.Write([]byte("$apr1$"))
	ha.Write([]byte(salt))

	// 把 db 循环填充到与 pw 等长
	for i := len(pw); i > 0; i -= 16 {
		n := i
		if n > 16 {
			n = 16
		}
		ha.Write(db[:n])
	}

	// 混淆：C 源码在这之前 memset(final,0)，所以 i 为奇数补 0x00，偶数补 pw[0]
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 == 1 {
			ha.Write([]byte{0})
		} else {
			ha.Write([]byte{pw[0]})
		}
	}
	da := ha.Sum(nil)

	// 1000 轮迭代
	digest := da
	for i := 0; i < 1000; i++ {
		h3 := md5.New()
		if i&1 == 1 {
			h3.Write([]byte(pw))
		} else {
			h3.Write(digest)
		}
		if i%3 != 0 {
			h3.Write([]byte(salt))
		}
		if i%7 != 0 {
			h3.Write([]byte(pw))
		}
		if i&1 == 1 {
			h3.Write(digest)
		} else {
			h3.Write([]byte(pw))
		}
		digest = h3.Sum(nil)
	}

	// 6) 按固定乱序取字节做 base64
	var out []byte
	for _, o := range [][3]int{{0, 6, 12}, {1, 7, 13}, {2, 8, 14}, {3, 9, 15}, {4, 10, 5}} {
		v := uint(digest[o[0]])<<16 | uint(digest[o[1]])<<8 | uint(digest[o[2]])
		out = append(out, apr1To64(v, 4)...)
	}
	out = append(out, apr1To64(uint(digest[11]), 2)...)
	return string(out)
}
