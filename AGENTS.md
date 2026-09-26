# AGENTS.md — Pipewright

自托管 CI/CD + 部署 + 运维平台。**单个静态 Go 二进制**（前端经 `go:embed` 内嵌，无 CGO、无运行时依赖）。面向用户的能力清单见 `README.md`（英文）/ `README.zh-CN.md`；本文件只写改代码需要知道的事。

## 命令

```bash
# 后端（纯 Go，无 CGO）
make build              # 前端构建 → go:embed → ./pipewright
make test               # go test . ./cmd/... ./internal/...
make vet                # go vet 同上三个包范围
make fmt-check          # gofmt -l cmd internal embed.go，有输出即失败
make mem-check          # 断言常驻内存 ≤100MB（NFR-4）

# 前端（在 web/ 下）
npm run dev             # Vite HMR，代理 /api /healthz 到 :8080
npm run lint            # eslint
npm run typecheck       # vue-tsc --noEmit
npm run test            # vitest
npm run e2e             # playwright
```

测试范围必须显式写成 `. ./cmd/... ./internal/...`，**不要用 `./...`**（会扫进 `web/node_modules`）。CI 与上述一致，见 `.github/workflows/ci.yml`。

提交前本地必须全绿：`go vet` / `go build` / `go test` / `gofmt -l`，前端 `lint` / `typecheck` / `test`。

## 架构

一个进程，约 45 个领域包。依赖方向单向：

```
cmd/pipewright/main.go   ← 唯一的装配层（含打破循环依赖的 adapter）
        │
internal/httpapi         ← 唯一接触 HTTP 的包；领域包注入此处（~188 路由）
        │
internal/<domain>        ← 领域包之间不互相 import HTTP，也不直接碰 *sql.DB
        │
internal/store           ← 唯一持有 *sql.DB 的包
```

README 的 Architecture 一节有按 foundation / pipelines / execution / delivery / observability 分组的完整包地图。几个关键点：

- `internal/store` 是唯一访问数据库的包，其它包通过本包定义的 repository 接口取数。**严禁**绕过它。
- SQLite 驱动固定为 `modernc.org/sqlite`（纯 Go）。**严禁**换成需要 CGO 的 `mattn/go-sqlite3` —— 全静态交叉编译是双运行模式的前提。
- `internal/dag` 是纯调度内核（无 I/O）；`internal/dagrun` 负责编排。改调度语义时保持这个分层。
- 平台自带的 cron 解析器、DAG 引擎、DNS provider 客户端、artifact/build 缓存、Caddyfile 渲染器都是**自研**的，刻意不引第三方库。

## 新增代码的落点

**加一条 API 路由**：`internal/httpapi/<area>.go` 写 handler，`router.go` 注册；新领域服务通过 `Option` 注入（`New(…, opts...)`），并在 `cmd/pipewright/main.go` 完成装配。领域逻辑不要写在 handler 里。

**加一次数据库变更**：在 `internal/store/migrations/sqlite/` 与 `internal/store/migrations/mysql/` **各加一份同名同号的 `NNNN_name.sql`**（当前两侧各 48 份、1:1 对应，需保持）。文件经 `go:embed` 内嵌，按版本号排序幂等应用。
- SQLite 走单事务；MySQL 逐句执行（DDL 隐式提交），幂等靠 `CREATE TABLE/TRIGGER IF NOT EXISTS`。
- MySQL 的 `CREATE INDEX` / `ALTER ADD COLUMN` 无 `IF NOT EXISTS`，靠 `schema_migrations` 保证只应用一次。

**跨库分歧 SQL**（upsert / 自增 / 标识符引用）：用 `internal/store/dialect.go` 的 helper（`UpsertSuffix` / `UpsertAssignSuffix` / `DoNothingSuffix` / `Excluded`），**不要**在领域包里 `if dialect == MySQL` 分叉。

**加服务端可翻译文案**：调用点用 zh-CN 源码字符串，`internal/i18n` 的 catalog 按 `init()` 里的 `register()` / `registerPrefix()` 注册（见 `messages_part*.go`）。拼接式消息（`"前缀:" + detail`）走前缀最长匹配，注册静态前缀即可。客户端通过 `X-Pipewright-Locale` 头传当前 UI 语言；未登记的字符串原样透传。

**加前端页面/文案**：页面在 `web/src/views/`，路由 `web/src/router/index.ts`，状态 `web/src/stores/`（Pinia），组件 `web/src/components/<area>/`。文案必须在 `web/src/i18n/locales/` 的 **8 个语言目录**下同步补齐 —— `keyParity.test.ts` / `componentKeys.test.ts` 会断言键齐全，缺一个就红。

## 硬规则

- **CI/CD 关键路径绝不依赖 AI**。AI 不可用时核心构建/部署必须优雅降级（NFR-10）。让 build/deploy 强依赖 AI 的改动会被拒。
- 凭据只以密文存在（NaCl secretbox 保险库），日志/诊断/通知统一走 `internal/mask` 脱敏。**不要**在任何输出路径漏明文。
- 命令一律数组化执行，防注入；出站请求有 SSRF 防护。
- 审计表是 append-only，由 SQLite trigger 硬阻断 UPDATE/DELETE。
- **禁止提交 lint 配置文件**（config-protection 钩子会拦）。这也是 CI 用内置 `gofmt` + `go vet` 而非 golangci-lint 的原因。
- 语言/工具链版本以 `go.mod`（Go 1.26+）与 `web/package.json` 为准。

## 测试约定

- 表驱动测试，与所在包同目录 `*_test.go`。跨方言夹具用 `internal/storetest`：设 `PIPEWRIGHT_TEST_MYSQL_DSN` 即整套切 MySQL，未设则只跑 SQLite（MySQL 子测试 `t.Skip`），并为每个 schema 建唯一库、`Cleanup` 时 DROP。
- 需要真实 Docker / SSH 的用例带 `_e2e_` 或 `realssh` 标记，CI 默认不阻塞。
- 断言行为与契约，不要断言实现细节、字段拷贝或源码文本。

## Git

- trunk 是 `master`，**永远保持可构建、CI 全绿**；**禁止直接 push**，一律走 PR。
- **一改动 = 一分支 = 一 PR**，分支名 `<type>/<kebab-desc>`（如 `fix/deploy-health-gate-timeout`），合并后删分支。
- Conventional Commits：`type(scope): description`，中文描述为主，例：`fix(run): 取消恰落在 worker 取活与转 running 之间时,run 永久卡 queued`。
- 新功能 / 架构调整 / 破坏性变更**先开 Issue 对齐**；安全漏洞走私密通道（`SECURITY.md`），不要开公开 Issue。
