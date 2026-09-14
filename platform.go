package main

import (
	"fmt"
	"strings"
)

// ============== 平台抽象 ==============
//
// platform 是客户端请求里的一个字段，缓存键形如 trans:mod:<platform>:<id>:<lang>，
// 所以"以后再加一个内容源"不用动表结构，只需要扩 platformSupported 与
// fetchModFullByPlatform 这两处。
//
// 当前只实现 Modrinth：
//
//	modrinth —— slug / nanoid 标识；/v2/project/{id} 一次请求同时拿到短描述 + 长正文，
//	长正文本身就是 Markdown，直接交给模型翻译即可。
//
// 关于 CurseForge（故意不做）：它的第三方 API 条款（CurseForge 3rd Party API Terms and
// Conditions，Overwolf）第 3.1(e) 条明确禁止 "save or cache any data obtained through the
// API or SDK"，而本项目的核心恰恰就是"长期缓存译文" —— 两者从根上冲突；条款另有
// 2.2（API Key 不得与第三方共享）、3.1（不得用于与 CF 竞争的产品）等约束，
// 官方申请页还写明审核时看"第三方分发作者作品时如何取得作者同意"。
// 所以本仓库不提供 CurseForge 实现；想接的话请自行实现并自行确认合规。

const platformModrinth = "modrinth"

// platformSupported 判断客户端传来的 platform 是否受支持。
// 其余取值一律 400，避免把任意字符串写进缓存 key。
func platformSupported(p string) bool {
	return p == platformModrinth
}

// supportedPlatforms 当前支持的内容源列表：预热的来源循环、分来源计数都用它
// —— 要新增一个内容源，只改这里与 fetchModFullByPlatform 两处。
func supportedPlatforms() []string {
	return []string{platformModrinth}
}

// platformProjectURL 返回项目原站地址（面板「打开原站」按钮用）。
// 未知平台返回空串，前端据此隐藏按钮。
func platformProjectURL(platform, modID string) string {
	if platform == platformModrinth {
		return "https://modrinth.com/mod/" + modID
	}
	return ""
}

// modRef 标识「某个平台上的某个项目」，是 Worker 任务与正文回访队列的最小单位。
// 带上平台之后，预热 / 正文回访 / 更新检测那几套逻辑不必再关心具体来源。
type modRef struct {
	Platform string
	ID       string
}

func (m modRef) String() string { return m.Platform + ":" + m.ID }

// parseModRef 解析 modRef 的字符串形式，兼容历史数据：
// 队列里老条目是裸 modID（没有冒号），一律按 Modrinth 处理
// （Modrinth 的 slug 里不含冒号，所以这个判断是无歧义的）。
func parseModRef(s string) modRef {
	if i := strings.Index(s, ":"); i > 0 {
		if p := s[:i]; platformSupported(p) {
			return modRef{Platform: p, ID: s[i+1:]}
		}
	}
	return modRef{Platform: platformModrinth, ID: s}
}

// fetchModFullByPlatform 按平台取「短描述 / 长正文 / 更新时间」。
// 三个返回值的语义与 Modrinth 的 description/body/updated 对齐，上层的缓存、更新检测、
// 反馈重译逻辑对任何平台都是同一套。
func fetchModFullByPlatform(platform, modID string) (string, string, string, error) {
	if platform == platformModrinth {
		return fetchModrinthFull(modID)
	}
	return "", "", "", fmt.Errorf("unsupported platform: %s", platform)
}
