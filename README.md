# QookiX Launcher 翻译缓存系统

源码：<https://github.com/ZhaYi-Miao/Qookix-Translate-Platform>

给 Minecraft 模组客户端用的自托管翻译缓存服务。客户端只报「是哪个 mod」，服务端去 Modrinth 取描述与正文、
翻成中文并保存结果，同一个 mod 只翻一次。

> 本仓库只支持 Modrinth，未实现 CurseForge，原因见文末。

## 功能

- 缓存优先：Redis（热）+ SQLite（持久），同一个 mod 的并发请求只翻一次
- 多模型兜底链：前一个失败自动换下一个；每个模型可单独设置每日上限、RPM、冷却时间与熔断阈值
- 长正文按长度分块并行翻译，失败只重翻失败的分块
- 后台任务：热门 mod 预热、正文失败回访、上游更新检测
- 运维面板（单 HTML 文件）：概览、后台任务、模型与配置、缓存浏览、反馈审核、运行日志
- AI 管家：把脱敏后的运行数据交给指定模型，给出配置调整建议，人工确认后才生效，可一键回滚
- 逐接口鉴权：管理接口默认需要密码，客户端接口默认公开

## 快速开始

需要 Go 1.22+ 与 Redis（没有 Redis 也能翻译和缓存，但统计与面板多数接口不可用）。

```bash
go build -o trans .
REDIS_ADDR=127.0.0.1:6379 LLM_API_KEY=sk-xxx LLM_MODELS=模型名 ./trans
curl 127.0.0.1:8080/health      # {"status":"ok"}
```

常用环境变量（面板里也能改，改完存 Redis）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | `8080` | 监听端口，只绑 127.0.0.1，外面套 nginx / Cloudflare |
| `REDIS_ADDR` / `REDIS_PASS` | `127.0.0.1:6380` / 空 | Redis 地址与密码 |
| `SQLITE_PATH` | `./cache.db` | SQLite 路径 |
| `LLM_API_KEY` | 空 | 必填：一个 OpenAI 兼容端点的 Key |
| `LLM_BASE_URL` | OpenAI | 该端点的 chat/completions 地址 |
| `LLM_MODELS` | 空 | 逗号分隔的模型名，按顺序组成兜底链 |
| `LLM_CONCURRENT` / `LLM_TIMEOUT_SEC` | `2` / `30` | 并发数与单次超时 |
| `WORKER_ENABLED` / `WORKER_BODY_CACHE` | `0` / `0` | 是否启用后台任务、是否连正文一起缓存 |
| `HTPASSWD_PATH` | `./trans.htpasswd` | 管理接口的账号密码文件（htpasswd 格式） |

模型链路、多个账号、RPM 等建议在面板「模型与配置」里配。

## 接口

面向客户端的接口共 5 个，字段与错误码见 [API.md](API.md)。

| 方法 | 路径 | 用途 | 默认鉴权 |
|---|---|---|---|
| GET | `/health` | 存活检查 | 公开 |
| POST | `/translate/mod` | 单个 mod 的译文（描述，可选正文） | 公开 |
| POST | `/translate/mods` | 批量取描述（≤5） | 公开 |
| POST | `/feedback/stale` | 上报「上游已更新，请重翻」 | 公开 |
| POST | `/feedback/quality` | 上报「翻译有问题」，进面板审核队列 | 公开 |

`text` 与 `body` 都是 Markdown，客户端一个渲染器就够。

## 部署

systemd 跑一个只监听 127.0.0.1 的进程，前面用 nginx 或 Cloudflare 反代到 `8080`，静态页传上去后注意权限。
改配置有两条路：面板保存，或直接改 Redis 的 `config:runtime`（重启后从 Redis 加载）。

## 目录

| 文件 | 职责 |
|---|---|
| `main.go` | 路由、翻译与缓存核心、模型兜底链、限速与熔断、缓存浏览 API |
| `platform.go` | 内容源（Modrinth）抽象与取数入口 |
| `worker.go` | 后台任务：预热、正文回访、更新检测、进度 |
| `config.go` / `stats.go` | 运行时配置；统计读写与区间汇总 |
| `advisor.go` / `live.go` | AI 管家；实时并发槽位 |
| `authmw.go` | 逐接口鉴权开关与 htpasswd 校验 |
| `index.html` + `_build/` | 面板（由 `_build/` 下分片组装，不要直接改 `index.html`） |

## 为什么没有 CurseForge

CurseForge 的第三方 API 条款第 **3.1(e)** 条禁止 `save or cache any data obtained through the API or SDK`，
而本项目的核心就是长期缓存译文，与这条直接冲突；条款同时限制 API Key 不得共享、不得用于竞争产品，
官方申请页也提到要说明「第三方分发作者作品时如何取得作者同意」。因此本仓库不提供 CurseForge 实现。
需要的话请自行实现并自行确认合规，`platform.go` 里的 `supportedPlatforms()` 与 `fetchModFullByPlatform()`
是扩展点。

## 许可

[MIT License](LICENSE) © 2026 ZhaYi
