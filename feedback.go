package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============== 用户反馈审核 ==============
//
// 客户端把「翻译有问题」的反馈 POST 到 /feedback/quality，落进 quality_feedback 表（status 默认 pending）。
// 这里提供面板侧的人工审核闭环：列表 -> 看译文 -> 重新翻译 / 标记已处理 / 忽略。
//
// 状态取值：
//
//	pending       待处理（新反馈；重译失败也会退回这里，方便重试）
//	retranslating 已排入重译队列，正在翻
//	done          已处理（重译成功 或 人工标记）
//	ignored       已忽略（不认为是问题）
//	failed        重译失败（模型链全挂 / Modrinth 取不到）
const (
	fbStatusPending       = "pending"
	fbStatusRetranslating = "retranslating"
	fbStatusDone          = "done"
	fbStatusIgnored       = "ignored"
	fbStatusFailed        = "failed"
)

// FeedbackItem 是面板「反馈审核」页的一行。
type FeedbackItem struct {
	ID             int64  `json:"id"`
	Platform       string `json:"platform"`
	ModID          string `json:"mod_id"`
	Lang           string `json:"lang"`
	IssueType      string `json:"issue_type"`
	UserSuggestion string `json:"user_suggestion"`
	UserComment    string `json:"user_comment"`
	IP             string `json:"ip"`
	CreatedAt      int64  `json:"created_at"`
	Status         string `json:"status"`
	// 当前缓存里这份译文的更新时间（0 = 还没缓存），审核时用来看"是否真的重译过"
	DescUpdatedAt int64 `json:"desc_updated_at"`
	BodyUpdatedAt int64 `json:"body_updated_at"`
}

// feedbackCounts 各状态条数，待处理那一档给侧边栏角标用。
func feedbackCounts() map[string]int64 {
	out := map[string]int64{}
	rows, err := db.Query(`SELECT status, COUNT(*) FROM quality_feedback GROUP BY status`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int64
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// cacheUpdatedAt 取某个缓存键的更新时间，没缓存返回 0。
func cacheUpdatedAt(key string) int64 {
	var ts int64
	if err := db.QueryRow(`SELECT updated_at FROM translations WHERE key = ?`, key).Scan(&ts); err != nil {
		return 0
	}
	return ts
}

func setFeedbackStatus(id int64, status string) {
	if id <= 0 {
		return
	}
	if _, err := db.Exec(`UPDATE quality_feedback SET status = ? WHERE id = ?`, status, id); err != nil {
		log.Printf("[FEEDBACK] 更新状态失败 id=%d: %v", id, err)
	}
}

// handleFeedbackList 列出反馈：status 过滤 + mod id 搜索 + 分页，附各状态计数。
func handleFeedbackList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}

	where := []string{"1=1"}
	args := []interface{}{}
	if status != "" && status != "all" {
		where = append(where, "status = ?")
		args = append(args, status)
	}
	if q != "" {
		where = append(where, "mod_id LIKE ?")
		args = append(args, "%"+q+"%")
	}
	base := strings.Join(where, " AND ")

	var total int64
	if err := db.QueryRow("SELECT COUNT(*) FROM quality_feedback WHERE "+base, args...).Scan(&total); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rows, err := db.Query(`SELECT id, platform, mod_id, lang, issue_type,
			COALESCE(user_suggestion,''), COALESCE(user_comment,''), COALESCE(ip,''), created_at, status
		FROM quality_feedback WHERE `+base+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	items := []FeedbackItem{}
	for rows.Next() {
		var it FeedbackItem
		if err := rows.Scan(&it.ID, &it.Platform, &it.ModID, &it.Lang, &it.IssueType,
			&it.UserSuggestion, &it.UserComment, &it.IP, &it.CreatedAt, &it.Status); err != nil {
			continue
		}
		it.DescUpdatedAt = cacheUpdatedAt(modCacheKey(it.Platform, it.ModID, it.Lang))
		it.BodyUpdatedAt = cacheUpdatedAt(modBodyCacheKey(it.Platform, it.ModID, it.Lang))
		items = append(items, it)
	}

	writeJSON(w, map[string]interface{}{
		"items":  items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
		"counts": feedbackCounts(),
	})
}

type feedbackStatusRequest struct {
	IDs    []int64 `json:"ids"`
	Status string  `json:"status"`
}

// handleFeedbackStatus 批量改状态：已处理 / 忽略 / 退回待处理。
func handleFeedbackStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req feedbackStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		http.Error(w, "ids are required", http.StatusBadRequest)
		return
	}
	switch req.Status {
	case fbStatusPending, fbStatusDone, fbStatusIgnored:
	default:
		http.Error(w, "status must be pending/done/ignored", http.StatusBadRequest)
		return
	}

	var updated int64
	for _, id := range req.IDs {
		res, err := db.Exec(`UPDATE quality_feedback SET status = ? WHERE id = ?`, req.Status, id)
		if err != nil {
			log.Printf("[FEEDBACK] 改状态失败 id=%d: %v", id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated += n
		}
	}
	log.Printf("[FEEDBACK] %d 条标记为 %s", updated, req.Status)
	writeJSON(w, map[string]interface{}{"updated": updated, "counts": feedbackCounts()})
}

type feedbackRetranslateRequest struct {
	IDs      []int64 `json:"ids"`
	ModID    string  `json:"mod_id"`
	Platform string  `json:"platform"`
	Lang     string  `json:"lang"`
}

// handleFeedbackRetranslate 重新翻译：清掉描述+正文缓存，丢进后台队列串行重译。
// 正文分块可能要几十秒，不能占着 HTTP 请求不放，所以立刻返回、结果写进日志与状态列。
func handleFeedbackRetranslate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req feedbackRetranslateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	startFeedbackQueue()

	queued := 0
	full := 0
	if len(req.IDs) > 0 {
		for _, id := range req.IDs {
			var modID, lang, platform string
			if err := db.QueryRow(`SELECT mod_id, lang, platform FROM quality_feedback WHERE id = ?`, id).Scan(&modID, &lang, &platform); err != nil {
				continue
			}
			if lang == "" {
				lang = "zh"
			}
			// 反馈表里存的是用户端上报的平台；老数据或脏数据按 Modrinth 处理
			if !platformSupported(platform) {
				platform = platformModrinth
			}
			if !enqueueFeedback(feedbackTask{id: id, platform: platform, modID: modID, lang: lang}) {
				full++
				continue
			}
			setFeedbackStatus(id, fbStatusRetranslating)
			queued++
		}
	} else if req.ModID != "" {
		// 面板「缓存浏览」也能直接重译一个 mod，这种没有反馈 id
		if req.Lang == "" {
			req.Lang = "zh"
		}
		if !platformSupported(req.Platform) {
			req.Platform = platformModrinth
		}
		if enqueueFeedback(feedbackTask{platform: req.Platform, modID: req.ModID, lang: req.Lang}) {
			queued++
		} else {
			full++
		}
	}

	if queued == 0 && full > 0 {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "重译队列已满，稍后再试", "backlog": len(feedbackQueue)})
		return
	}
	if queued == 0 {
		http.Error(w, "no valid target", http.StatusBadRequest)
		return
	}
	log.Printf("[FEEDBACK] 已排入重新翻译队列 %d 个（积压 %d）", queued, len(feedbackQueue))
	writeJSON(w, map[string]interface{}{"queued": queued, "backlog": len(feedbackQueue), "counts": feedbackCounts()})
}

// ============== 后台重译队列 ==============

type feedbackTask struct {
	id       int64
	platform string
	modID    string
	lang     string
}

var (
	feedbackQueue     chan feedbackTask
	feedbackQueueOnce sync.Once
)

// enqueueFeedback 非阻塞入队，队列满返回 false（宁可提示用户稍后重试，也不让请求卡住）。
func enqueueFeedback(t feedbackTask) bool {
	if feedbackQueue == nil {
		return false
	}
	select {
	case feedbackQueue <- t:
		return true
	default:
		log.Printf("[FEEDBACK] 队列已满，放弃 %s", t.modID)
		return false
	}
}

// startFeedbackQueue 串行消费重译任务：一次只翻一个 mod，避免和 Worker 抢模型额度。
func startFeedbackQueue() {
	feedbackQueueOnce.Do(func() {
		feedbackQueue = make(chan feedbackTask, 200)
		go func() {
			for t := range feedbackQueue {
				start := time.Now()
				log.Printf("[FEEDBACK] 开始重新翻译 %s (%s)", t.modID, t.lang)
				if err := retranslateForReview(t.platform, t.modID, t.lang); err != nil {
					log.Printf("[FEEDBACK] %s 重新翻译失败: %v", t.modID, err)
					setFeedbackStatus(t.id, fbStatusFailed)
					continue
				}
				log.Printf("[FEEDBACK] %s 重新翻译完成，用时 %dms", t.modID, time.Since(start).Milliseconds())
				setFeedbackStatus(t.id, fbStatusDone)
			}
		}()
	})
}

// retranslateForReview 强制重翻描述+正文（不受 Worker 的 body_cache 开关影响）：
// 用户报的是质量问题，正文同样要刷新，否则审核完看到的还是旧译文。
//
// 注意：Worker 满载预热时整条模型链可能都在限速冷却，一次就成的概率不高，
// 所以要带退避重试；重试时描述已缓存就不再翻一遍，省掉一次调用。
func retranslateForReview(platform, modID, lang string) error {
	if !platformSupported(platform) {
		platform = platformModrinth
	}
	cacheDelete(modCacheKey(platform, modID, lang))
	cacheDelete(modBodyCacheKey(platform, modID, lang))

	desc, body, updated, err := fetchModFullByPlatform(platform, modID)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", platform, err)
	}
	if desc == "" {
		return fmt.Errorf("empty desc for %s", modID)
	}

	const attempts = 3
	backoffs := []time.Duration{15 * time.Second, 30 * time.Second}
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			wait := backoffs[len(backoffs)-1]
			if attempt-1 < len(backoffs) {
				wait = backoffs[attempt-1]
			}
			log.Printf("[FEEDBACK] %s 重试 %d/%d，等 %v 让模型链冷却", modID, attempt+1, attempts, wait)
			time.Sleep(wait)
		}

		// 描述已缓存（上一轮翻成功过）就不再翻一遍
		needDesc := true
		if attempt > 0 {
			if entry, ok := cacheGetEntry(modCacheKey(platform, modID, lang)); ok && entry.Text != "" {
				needDesc = false
			}
		}

		lastErr = nil
		if needDesc {
			// 描述也按 body_share 权重分摊，避免全压给链首账号
			steps := bodyChain(getCfg(), modID)
			descCall := func(text, lg string) (string, error) {
				return callChainIn(getCfg(), text, lg, steps, true, "desc")
			}
			if _, err := translateAndCacheDesc(platform, modID, lang, desc, updated, descCall); err != nil {
				lastErr = fmt.Errorf("desc: %w", err)
				continue
			}
		}

		if body != "" {
			if _, err := translateAndCacheBody(platform, modID, lang, body, updated, getCfg(), true); err != nil {
				lastErr = fmt.Errorf("body: %w", err)
				continue
			}
		}
		return nil
	}
	return lastErr
}
