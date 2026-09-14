# QookiX 翻译缓存服务 · 客户端接口文档

> 面向调用方（启动器 / 模组）的完整接口说明。
> 最后更新：2026-09-14（本次变更见文末「变更记录」）

本服务为**只读的翻译缓存**：客户端给出「平台 + 项目标识」，服务端返回已缓存的译文；
未命中时服务端会**同步**去原站拉内容并调用模型翻译，然后写缓存。

---

## 1. 接入信息

| 项目 | 值 |
|---|---|
| Base URL | `https://trans.example.com` |
| 传输 | HTTP/2、HTTPS、UTF-8、`Content-Type: application/json` |
| 鉴权 | 客户端接口（`/translate/*`、`/feedback/*`、`/health`）**默认公开，无需鉴权** |
| 请求体上限 | 64 KB |

> ⚠️ **必须通过 `https://trans.example.com` 访问。**
> 源站（IP:8080）只允许 Cloudflare 回源，直连 IP 会得到 `403 Forbidden`，这是有意配置，不是故障。

---

## 2. 平台与项目标识（`platform` / `mod_id`）— **最重要的一节**

```json
{ "platform": "modrinth", "mod_id": "sodium" }
```

| `platform` | `mod_id` 允许的形式 | 示例 |
|---|---|---|
| `modrinth` | slug 或 project id（nanoid） | `sodium`、`AANobbMI` |

> 本仓库只实现 **Modrinth** 一个内容源，`platform` 目前只接受 `modrinth`，其它取值一律 `400`
> （为什么不做 CurseForge：它的第三方 API 条款禁止缓存 API 返回的数据，见 README）。

规则与注意事项：

1. **只传标识本身**，不要传完整网址（不要 `https://modrinth.com/mod/sodium`）。
2. `platform` 是**必填**，缺失或非法都返回 `400`。
3. **缓存键带平台**：`trans:mod:<platform>:<mod_id>:<lang>` —— 将来加内容源时也不会互相覆盖。
4. `lang`：建议始终显式传 `zh`。不传时服务端按 `zh` 处理；
   该字段是缓存键的一部分，传不同的值会产生一份独立缓存，**不要传空串以外的随意值**。

---

## 3. 接口一览

| 方法 | 路径 | 用途 | 默认鉴权 |
|---|---|---|---|
| GET | `/health` | 存活检查 | 公开 |
| POST | `/translate/mod` | 取单个项目的译文（描述，可选正文） | 公开 |
| POST | `/translate/mods` | 批量取描述（≤5 个，同平台） | 公开 |
| POST | `/feedback/stale` | 上报「内容已过时，请重新翻译」 | 公开 |
| POST | `/feedback/quality` | 上报「译文质量问题 + 用户建议」 | 公开 |

---

## 4. `GET /health`

**响应** `200`

```json
{ "status": "ok" }
```

---

## 5. `POST /translate/mod`

### 请求

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `platform` | string | ✅ | 目前只有 `modrinth` |
| `mod_id` | string | ✅ | 见第 2 节 |
| `lang` | string | ⬜ | 默认 `zh` |
| `include_body` | bool | ⬜ | 是否同时要长正文（**很慢**，见第 8 节）。默认 `false` |

```json
{
  "platform": "modrinth",
  "mod_id": "sodium",
  "lang": "zh",
  "include_body": false
}
```

### 响应 `200`

| 字段 | 类型 | 说明 |
|---|---|---|
| `cached` | bool | `true` = 服务端缓存命中（快）；`false` = 本次真的调了模型（慢） |
| `text` | string | 描述译文。**出错时该字段不出现** |
| `body` | string | 正文译文（仅当 `include_body:true` 且成功） |
| `body_cached` | bool | 正文是否来自缓存（仅当请求了正文时出现） |
| `error` | string | 出错说明，出现即表示本次失败 |

### `text` / `body` 的格式契约（**请务必按此渲染**）

**两个字段都是 Markdown**：

- `modrinth`：原站本来就是 Markdown，原样保留（`## 标题`、`- 列表`、`**加粗**`、代码块、图片）。

所以客户端**只需要一个 Markdown 渲染器**，不要把 `body` 当纯文本直接显示
（否则 `##`、`**` 会以字面出现，或者段落全部糊在一起）。

成功示例：

```json
{ "text": "查看物品和配方", "cached": true }
```

带正文成功示例：

```json
{ "text": "查看物品和配方", "cached": true,
  "body": "## 简介\n…", "body_cached": false }
```

描述成功但正文失败（HTTP 仍为 `200`，看 `error`）：

```json
{ "text": "钠 · 现代渲染引擎", "cached": true,
  "error": "body failed: fetch modrinth failed: upstream 404 not found" }
```

### 响应 `500`（本次请求失败）

```json
{ "cached": false, "error": "fetch modrinth failed: upstream 404 not found" }
```

常见 `error` 文案（**按关键字判断，不要整体匹配**）：

| error 中包含 | 含义 | 客户端建议 |
|---|---|---|
| `upstream 404 not found` | 该平台没有这个项目（或 slug 写错） | 不要重试，标记该项目不可用 |
| `upstream rate limited` | 上游模型账号被限速 | 等 ≥5 秒再试 |
| `empty description` | 项目存在但没有可翻译的描述 | 不要重试 |
| `fetch ... failed: after 3 attempts` | 上游反复异常 | 可稍后重试 |

> 注意：目前「项目不存在」返回的是 **500**（错误文本里带 `404`），不是 404 状态码。

---

## 6. `POST /translate/mods`（批量）

一次最多 **5** 个，**同一平台**。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `platform` | string | ✅ | 整批共用一个平台 |
| `mod_ids` | string[] | ✅ | 1~5 个标识 |
| `lang` | string | ⬜ | 默认 `zh` |
| `include_body` | bool | ⬜ | 会给**每个**项目都拉正文，慎用 |

```json
{ "platform": "modrinth", "mod_ids": ["sodium", "lithium", "ferritecore"], "lang": "zh" }
```

### 响应 `200`

`results` 的 key 是**你传入的原始字符串**（传 slug 就还是 slug，不会变成数字 id）：

```json
{
  "results": {
    "sodium":      { "text": "一款用于 Minecraft 的高性能渲染引擎…", "cached": true },
    "lithium":     { "text": "…", "cached": false },
    "ferritecore": { "cached": false, "error": "fetch modrinth failed: upstream 404 not found" }
  }
}
```

> 批量的 HTTP 状态码一般为 `200`，**单个项目失败不影响整批**，逐个看 `results[*].error`。

---

## 7. `POST /feedback/stale`（内容过时，请求重译）

客户端发现「原站的更新时间变了」时上报，服务端会去核对并重译（成功后强制刷新缓存）。

```json
{ "platform": "modrinth", "mod_id": "sodium", "lang": "zh" }
```

**响应始终是 `200` + `{"status": "..."}`**（除服务端异常为 500）：

| `status` | 含义 | 客户端应对 |
|---|---|---|
| `updated` | 确实过时，已重新翻译并刷新缓存 | 提示用户，稍后重新取译文 |
| `unchanged` | 原站没有更新 | 无需处理 |
| `no_cache` | 服务端没有该项目的缓存（或标识无法解析） | 先调 `/translate/mod` |
| `cooldown` | 该项目的刷新冷却中（同一项目 24 小时内只受理一次） | 无需处理 |
| `received` | 已收到（**也可能是被限流静默丢弃**，见第 9 节） | 无需处理 |
| `error` | 核对上游时出错（HTTP `500`） | 稍后重试 |
| `refresh_failed` | 重译失败（HTTP `500`） | 稍后重试 |

---

## 8. `POST /feedback/quality`（译文质量反馈）

```json
{
  "platform": "modrinth",
  "mod_id": "sodium",
  "lang": "zh",
  "issue_type": "wrong_translation",
  "user_suggestion": "建议译为「钠」而不是「钠元素」",
  "user_comment": "可选补充"
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `platform` / `mod_id` / `lang` | string | ✅/✅/⬜ | 同前，`lang` 默认 `zh` |
| `issue_type` | string | ✅ | 只能是 `wrong_translation` / `unnatural` / `missing` / `other` |
| `user_suggestion` | string | ⬜ | ≤ **500 字节** |
| `user_comment` | string | ⬜ | ≤ **500 字节** |

> ⚠️ **长度按字节算**（不是字符）：500 字节 ≈ 166 个汉字。超长返回 `400 user_suggestion too long (max 500)`。

**响应**：`200 {"status":"received"}`；`400` 参数不合法；`500 {"status":"error"}` 入库失败。

---

## 9. 限流（**按客户端 IP**）

| 接口 | 速率 | 突发 | 超限时表现 |
|---|---|---|---|
| `/translate/mod` | 30 次/分 | 10 | `429 {"error":"rate limit exceeded"}` |
| `/translate/mods` | 30 次/分 | 10 | 同上；**按条数计费**（一次 5 个 = 消耗 5 次额度） |
| `/feedback/stale` | 10 次/天 | 3 | `200 {"status":"received"}`（**静默丢弃**） |
| `/feedback/quality` | 5 次/天 | 2 | `200 {"status":"received"}`（**静默丢弃**） |

要点：
- 限流是按 **IP** 的。若启动器把用户流量中转自同一出口 IP，请自行做客户端侧的请求节流/合并。
- 反馈类接口超限时**不报错**（防止被刷），客户端无法区分「已受理」与「被丢弃」。所以**不要依赖反馈接口的返回值做业务判断**。
- 批量接口按条数扣额度，一次请求 5 条就是 5 次；建议用它替代循环调用单接口。

---

## 10. 缓存与延迟语义（客户端体验的关键）

1. **同步翻译**：未命中（`cached:false`）时，服务端会在这次 HTTP 请求里完成「拉原站 + 调模型」，所以：
   - 描述：通常 **1~3 秒**，上游排队严重时可能十几秒；
   - 正文（`include_body:true`）：**可能 10~60 秒**（按 8000 字符分块翻译，最多 12000 字符）。
   → **不要在列表页/批量刷新里请求正文**；正文只在用户点开详情时按需请求。
2. **缓存有效期 30 天**，命中后响应通常在毫秒级。
3. **并发合并（single-flight）**：同一 `platform+mod_id+lang` 的并发请求，服务端只真正翻译一次，
   其余共享同一结果。所以客户端**可以放心并发**请求多个不同项目。
4. `cached` 只描述「描述」这条；正文看 `body_cached`。
5. 服务端**不会因为客户端请求就立刻删除缓存**（只有 `/feedback/stale` 核对通过才刷新）。

---

## 11. 状态码约定汇总

| 状态码 | 场景 | 响应体 |
|---|---|---|
| `200` | 成功；或反馈已收到/被静默丢弃 | JSON（见各接口） |
| `400` | 参数不合法：缺字段、不支持的 platform、`mod_ids` 为空或超过 5 个、反馈文本超长 | **纯文本**错误说明，不是 JSON |
| `401` | 该接口被管理员设为需要鉴权 | 纯文本 `401 unauthorized` |
| `429` | 翻译接口限流 | `{"error":"rate limit exceeded"}` |
| `500` | 上游/翻译失败：项目不存在、被限速、正文失败等 | `{"error":"..."}`（`/translate/mod`）；反馈接口为 `{"status":"..."}` |

> 客户端解析错误时要兼容「纯文本回包」——先看 `Content-Type`，不要直接 `JSON.parse`。

---

## 12. 集成建议

1. **先描述、后正文**：列表用 `/translate/mods` 拿描述；详情页再带 `include_body:true` 取正文。
2. **本地也缓存一份**：把 `text`/`body` 连同 `platform + mod_id + lang` 存在客户端自己的库里
   （服务端数据 30 天有效，但用户断网时你仍要能显示）。
3. **遇到 `cached:false` 时给用户加载态**，并设置 **≥60 秒**的 HTTP 超时（正文请求建议 ≥120 秒）。
4. **重试策略**：只对 `5xx` 和 `429` 重试，间隔 ≥5 秒、最多 2 次；`404 not found` / `empty description` 不要重试。
5. **带上可识别的 User-Agent**（如 `QookiXLauncher/1.2 (+https://…)`），便于排查问题。
6. **上报过时**用 `/feedback/stale`（用户主动点「检查更新」时），不要每次启动都刷。

---

## 13. cURL 速查

```bash
# 健康检查
curl https://trans.example.com/health

# Modrinth（slug）
curl -s -X POST https://trans.example.com/translate/mod \
  -H 'Content-Type: application/json' \
  -d '{"platform":"modrinth","mod_id":"sodium","lang":"zh"}'

# 带正文（很慢）
curl -s -X POST https://trans.example.com/translate/mod \
  -H 'Content-Type: application/json' \
  -d '{"platform":"modrinth","mod_id":"sodium","lang":"zh","include_body":true}'

# 批量（≤5，同平台）
curl -s -X POST https://trans.example.com/translate/mods \
  -H 'Content-Type: application/json' \
  -d '{"platform":"modrinth","mod_ids":["sodium","lithium"],"lang":"zh"}'

# 上报更新
curl -s -X POST https://trans.example.com/feedback/stale \
  -H 'Content-Type: application/json' \
  -d '{"platform":"modrinth","mod_id":"sodium","lang":"zh"}'
```

### Java（OkHttp）骨架

```java
OkHttpClient http = new OkHttpClient.Builder()
    .connectTimeout(10, TimeUnit.SECONDS)
    .readTimeout(120, TimeUnit.SECONDS)   // 正文翻译可能很慢
    .build();

String body = "{\"platform\":\"modrinth\",\"mod_id\":\"sodium\",\"lang\":\"zh\"}";
Request req = new Request.Builder()
    .url("https://trans.example.com/translate/mod")
    .header("Content-Type", "application/json")
    .header("User-Agent", "QookiXLauncher/1.0")
    .post(RequestBody.create(body, MediaType.parse("application/json")))
    .build();

try (Response resp = http.newCall(req).execute()) {
    String s = resp.body().string();
    if (resp.code() == 200) {
        JSONObject o = new JSONObject(s);
        String text = o.optString("text", null);
        String err  = o.optString("error", null);
        // text 可能为 null（该项目不可翻译）；err 非空表示失败
    } else if (resp.code() == 429) {
        // 限流：退避重试
    } else {
        // 400 是纯文本；500 是 {"error": "..."}
    }
}
```

---

## 14. 变更记录

| 日期 | 变更 | 对客户端的影响 |
|---|---|---|
| — | `platform` 字段变为**必填**（缺失/非法返回 400） | 必须显式传平台，不能再只传 `mod_id` |
| — | 缓存键统一为 `trans:mod:<platform>:<mod_id>:<lang>` | 按平台隔离，将来加内容源不会互相覆盖 |
| — | `text` / `body` 统一为 **Markdown** | 客户端只需要一个 Markdown 渲染器 |
