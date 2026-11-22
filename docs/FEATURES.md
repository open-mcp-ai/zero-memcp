# 功能矩阵与已知限制

> 这份文件以**代码为准**，不是设计意图。
> 每条能力都标注「入口」与「验证」：验证写的是"靠什么证明它能用"，
> 没有证据的一律标为未验证，不写成"已支持"。
>
> 最后核对：2026-11-22（命令流同步 + remind 幂等落地之后）

---

## 一、运行形态

| 能力 | 入口 | 状态 | 验证 |
|---|---|---|---|
| 前台运行 | `zero-memcp serve -data DIR [-config FILE] [-listen ADDR]` | 已实现 | `cmd/zero-memcp/main.go` |
| 兼容旧写法 | `zero-memcp -data DIR ...`（等同 `serve`） | 已实现 | 同上 |
| 后台守护 | `zero-memcp daemon start/status/stop` | 已实现 | `cmd/zero-memcp/daemon_cmd.go` |
| 版本 | `zero-memcp version`（构建期注入 `-X main.version`） | 已实现 | CI 实测：不注入时输出 `dev`，注入后与 tag 一致 |
| 帮助 | `zero-memcp help` | 已实现 | — |
| 单实例互斥 | `<data>/system/instance.lock` 内核文件锁 | 已实现 | 进程崩溃由内核自动释放，无陈旧锁 |
| 管理台（WebUI） | `/`、`/ui/*`，口令会话 cookie | 已实现 | 首次启动打印一次性口令；`system/ui.passwd` 存 argon2id |

**CLI 不是数据入口**：终端读写记忆会绕过凭证体系，`Caller` 会退化成写死的 root。
凭证签发、临时授权、打包都在管理台做，CLI 只负责起服务与诊断。

---

## 一之五、跨机器一致性：命令流（op log）

**这是唯一的内部同步抽象。** 工作层的增量改动与历史层的整树同步，
都是同一份命令流里的命令，区别只是粒度。

```
第 0 层  内容寻址对象库：HasObject / BlobBody / sha1 验真
第 1 层  树的命令流：set / delete / rename / copy / tree（批量）
          每条带单调位点 seq，可增量、可去重、可重放
第 2 层  blob 按需补齐：由 Missing 驱动，验真后重放被卡住的命令
第 3 层  对外 Git 适配器：仅在对接普通 Git 仓库时启用
```

| 能力 | 入口 | 状态 | 验证 |
|---|---|---|---|
| 单条写入登记位点 | `write` / `put` / `apply` 自动落日志 | 已实现 | `TestOpLogAssignsMonotonicSeqAndPersists` |
| 删除是命令而非特例 | `delete` 记 `opDelete` | 已实现 | `TestOpsSyncDeleteIsACommand` |
| 世界树操作进命令流 | `mv` → `opRename`，`cp` → `opCopy`（同分支）；跨分支记 set+delete | 已实现 | 见 `surgery.go` 注释；rename 在一条命令内原子生效 |
| 增量导出 | `/sync/ops/pull` 带 `after` 位点 | 已实现 | `TestOpLogIncrementalExport` |
| 推 / 拉共用落地路径 | `applyOps` + `commitOpsLocal` | 已实现 | `TestOpsSyncUncommittedAcrossNodes` |
| **整树同步 = 批量命令** | `opTree` 携带整棵树的条目 | 已实现 | `TestTreeSyncAsBatchOp`（空节点收一棵树） |
| 位点持久化 | `<data>/system/oplog/<node>/<project>/<branch>.ndjson` | 已实现 | 重启后续编号，位点回退即报错 |
| 缺口不静默 | 日志里位点不推进 → `Load` 报错并指出行号 | 已实现 | `TestOpLogRejectsCorruptSeq` |
| 对端身份学习 | peerName ↔ 日志属主名映射 | 已实现 | 修掉「每轮从头拉、重复应用」的键不一致 |
| 对象验真 | 收到的 blob 重算 sha1，不符即拒绝 | 已实现 | 沿用 `applyWorkBlob` |

**落地顺序是硬约束：先 tree，后 blob。** 缺对象的命令不落地，
记进 Missing——写一条指向空气的条目，会在一次毫不相干的 recall 上炸开。

实测（2026-11-22，本机双实例 17311/17312）：

```
A 写入不 commit → B 日志：
  applied 0, missing 1     ← 第一趟：命令到，对象缺，不落地
  applied 1, missing 0     ← 第二趟：对象补齐验真，重放成功
位点序列：1 set hello.md / 2 set second.md / 3 set third.md / 4 delete second.md
B 端：hello.md 与 third.md 可读，second.md 已被删除
反向 B 写 → A 可读；全程 commit 数为 0
```

---

## 二、MCP 动词（`term({cmd, vars|args})`）

原则：**声明面等于能力面**，能力增长发生在命令空间，不发生在 `tools/list`。

### 检索与读取

| 动词 | 用法 | 状态 |
|---|---|---|
| `recall` | `recall <q> [--scope T] [--bare] [--all] [--json]` | 已实现，BM25 覆盖全部可索引块 |
| `srecall` | `srecall <q> [--scope T] [--json]` | 已实现，需配置嵌入后端；未配置时返回明确错误 |
| `read` | `read <addr> [<addr>...]` | 已实现 |
| `grep` | `grep <pat> [--scope T] [--all] [--bare]` | 已实现，正文缓存命中后为毫秒级 |
| `log` / `ls` | `log [N]` / `ls [date] [--scope T]` | 已实现 |
| `trees` | `trees <project>[@<branch>]` | 已实现 |
| `status` | `status` | 已实现；含索引规模、工作层冲突、同步水位、正文缓存占用 |

### 写入

| 动词 | 用法 | 状态 |
|---|---|---|
| `put` | `put [<tree>] [--session id] $TEXT` | 已实现；正文必须走变量引用 |
| `ref` | `ref <tree> <addr>` | 已实现，不复制正文 |
| `write` | `write <p>@<br>/<path> $TEXT` | 已实现，精确路径写入 |
| `apply` | `apply <p>[@<br>] $PATCHES` | 已实现，一次往返多文件改写 |

### 分支与结构

`commit` / `pending` / `diff` / `branch` / `tag` / `cp` / `mv` / `delete` /
`archive` / `reset` / `lock` / `unlock` / `chmod` / `deleted` / `merge` / `merged` /
`ahead` / `modified` / `added` / `conflict` — 均已实现（见 `internal/server/term_verbs.go`）。

### 抹除（世界树改写）

`erase` / `replace`，可再叠加票据门（`[erase] public_key` 或 `require_ticket`）。

**边界**：只对本机与自控节点承诺彻底抹除。外部托管（GitHub 等）在对方 GC 前
仍可按 SHA 访问旧对象，且任何曾经的 clone 永久持有旧 blob。这是所有权问题，不是技术问题。

### 同步

| 动词 | 用法 | 状态 |
|---|---|---|
| `remote` | `remote [<project>]` | 已实现 |
| `fetch` / `push` / `sync` / `merge` | 见 `help` | 已实现，不依赖系统 Git |
| `remote-key` | `list / put --name N $KEY / delete N` | 已实现（root） |
| `remote-secret` | `list / put --name N $TOKEN / delete N` | 已实现（root） |
| `config` / `reload` / `whoami` / `users` / `acl` | 见 `help` | 已实现 |

### 组合语句

`;` 顺序执行；`&&` 上一条**查到内容**才继续；`||` 上一条没查到才继续。
判据是"命中"而非退出码——搜不到不是错误。

---

## 三、工作层同步（已落地）

**核心事实**：工作层（原脏层）是节点间同步的**主体**，commit 降级为历史书签。
两台机器写完直接互相可见，不需要先 commit。

| 能力 | 入口 | 状态 |
|---|---|---|
| 工作层导出/导入 | `POST /sync/work/pull`、`/sync/work/push` | 已实现 |
| 按需取正文 | `GET /sync/work/blob`、`POST /sync/work/blob/put` | 已实现，接收端自算 SHA-1 验真 |
| 先 tree 后 blob | `importWorkDelta` 先落墓碑与登记，再回头取缺失 blob | 已实现 |
| **推（本地 → 对端）** | `[[remote]] push = [...]` → `syncWorkWithRemote` | 已实现 |
| **拉（对端 → 本地）** | `[[remote]] pull = [...]` → `pullWorkFromPeer` | 已实现（2026-11-22 补） |
| **未提交内容的判重** | `dropAlreadyInSync` 剔除已一致条目 | 已实现，避免每轮重复应用 |
| **手动触发双向同步** | `sync <project> [remote]` | 已实现，工作层对端直接跑一次真实推拉 |
| 水位 | `out` 仅在对端回执后推进；`in` 记录拉取；`status` 显示 | 已实现 |
| **`remind` 看得见工作层** | 未提交分支报 `work layer only`；已提交分支有新条目报 `work layer modified` | 已实现（2026-11-22 补） |
| **`remind` 幂等** | 重复登记保留原基线；`--reanchor` 才推到当前头 | 已实现，`TestRemindKeepsBaselineOnRepeat` / `TestRemindReanchorResetsBaseline` |
| **关注表跨重启** | `<data>/system/watches.json` 落盘 + 启动加载 | 已实现，`TestWatchesSurviveServerRestart` |
| 同路径并发改写 | 保留本地、记录冲突、`status` 点名 | 已实现，`TestWorkLayerConcurrentRewriteReportsConflict` |
| 对端删除 vs 本地改写 | **删除优先**（防复活），但记录被解链的本地 SHA 并点名 | 已实现，`TestWorkLayerDeleteOverLocalWriteIsReportedNotSilent` |
| 落地即广播 | 写入/删除立即 `notifications/resources/updated` | 已实现 |
| 三节点环形收敛 | — | 已实现，`three_nodes_sync_test.go`（perf 标签） |

### 真实边界

- **`remind` 没有「收件人」字段**：关注表的键是凭证身份，「谁关心什么」由身份决定，不在地址里写 `@某人`。身份即收件人，这是设计而非缺漏。
- **重复 `remind` 不再重置基线**：agent 每次启动都会重登配置里的 watch，若每次都把基线推到「此刻」，停机期间对端写的东西会被当场抹平。要「我处理完了，从现在开始算」，用 `remind <tree> --reanchor`。
- **清空关注只认精确写法**：`remind -` / `remind --clear` / `remind -c`。早先用 `HasPrefix(rest, "-")` 判断，结果 `remind --reanchor`（无地址）被当成清空，把一个身份的关注表抹平。


- **自动同步是显式选择**：远端要写 `auto_sync = true` 才会进后台循环。
- **对端判定靠 URL 形状**：`http(s)://` 且不以 `.git` 结尾才当作工作层对端。
- **`pull` 只对工作层对端有意义**：写在 `.git` 远端上会在配置解析期报错（那边靠 fetch）。
- **`pull` 依赖 `auto_sync`**：只写 `pull` 不写 `auto_sync` 会在配置解析期报错，避免永不执行且静默。
- **`pull` 缺省时回落到 `push`**：只写 `push` 的对等配置，默认按同一批分支双向对齐。
- **`on_receive` 依赖 `auto_sync`**：只写 `on_receive` 会在配置解析期直接报错。
- **冲突需要人裁决**：冲突只记录与上报，不自动合并；`conflict clear` 清理。
- **水位是进程内状态**：重启后从 0 重新累计，语义是"本轮推进到哪"而非持久账本。

---

## 四、存储与持久化

| 能力 | 状态 | 说明 |
|---|---|---|
| Git 裸仓库为物理存储 | 已实现 | 每篇记忆一个 blob，每次封箱一个 commit |
| 工作层覆盖 | 已实现 | `dirty/<branch>/@<dir>.dirty`，纯文本可 cat/diff |
| 折叠转正 | 已实现 | 按目录深度自底向上重建 tree，原子推进 ref |
| 写入队列背压 | 已实现 | 队列满时等待重试（上限 5s），不再直接拒绝 |
| 批量刷盘 | 已实现 | 每 2 秒一次；Windows 上曾长期失败，已修 |
| 内容寻址缓存 | 已实现 | `bodyCache`（blob SHA）、`treeCache`（tree SHA），无失效逻辑 |
| 索引快照 | 已实现 | 落盘 + 差量补丁 + 计数漂移自愈 |
| 松散对象打包 | 已实现 | 管理台触发或 `[gc] interval` 周期；不做也是正确的 |
| packfile 读取 | 已实现 | 不做则 gc 之后记忆"凭空消失" |

### 真实边界

- **刷盘间隔 2 秒**：进程崩溃不丢（页缓存仍在），断电最多丢 2 秒写入。
- **对象写入不回读校验**：`writeObject` 见到文件已存在即信任。
  静默半截对象理论上仍可能被信任——设计文档中列为待办，本文不写成已实现。
- **工作层损坏行跳过**：脏文件尾部半截行会被跳过而非报错，
  代价是"丢一条改动"，换取"分支仍可打开"。

---

## 五、索引与检索

| 能力 | 状态 | 说明 |
|---|---|---|
| BM25 倒排 | 已实现 | 内存态；启动可由快照恢复，否则全量重建 |
| 语义向量 | 已实现 | 需配置 provider；多模型各自独立索引目录 |
| RRF 排名融合 | 已实现 | 多模型结果只在排名层融合，向量空间不跨模型比较 |
| 正文缓存预热 | 已实现 | 8 worker 后台补齐，实测 39k 块约 11 秒 |
| 索引热插拔 | 已实现 | 写入后立即刷新该路径，不必等 commit |

### 语义索引收纳范围（与旧文档不同）

参与语义索引的路径：`thought/`、`record/`、`review/`、`fact/`。
另外为历史数据补票：正文 frontmatter 里声明 `layer: thought|record|review|fact` 也纳入。

排除项优先：`me/` 与隐藏内容一律不进。

**为什么 `docs` 与 `semantic` 计数常不等**：两者收纳范围不同，
差的那些不属于语义层，不是丢数据。

---

## 六、权限

三层各自收窄，任一层说不行就是不行：

1. **端口**（`[[listen]] allow / mount / require`）
2. **配置**（`[[project]] mode/owner/group` + `[[access]]`）
3. **凭证**（JWT 的 `cap` / `mount` / `gated`）

| 能力 | 状态 |
|---|---|
| 能力门（gated）+ 临时授权 | 已实现，按 (JTI, 项目, 分支) 存储，一次判定读一次盘 |
| 挂载表 chroot | 已实现，表外不可见，匹配不到即拒绝 |
| 凭证吊销（含级联） | 已实现 |
| 会话与抽凭分离 | 已实现：两种凭据互相拒绝，MCP 侧在鉴权前检查会话 cookie |
| 规则注入判定 | 已实现：`SKILL.md` frontmatter 的 `inject: every_turn` 优先，mode 位回退，非 Markdown 一律不是规则 |
| 主钥目录热投放 | 已实现（`<data>/keys/*.toml`，一钥一文件） |
| 世界树改写票据 | 已实现（可选开启） |

### 真实边界

- **`enforce_auth = false` 时，本机任意进程不带凭证即被视为 root。**
  多用户机器或容器里必须打开。
- **Unix 权限位校验只在有意义的平台生效**：Windows 上 `Perm()` 由只读属性合成，
  真实保护要靠 NTFS ACL；代码会在此时明确说明这一点，不假装验过。
- **root 判定只来自 `[[user]] root = true`**，不来自名字、能力位或通配符。

---

## 七、嵌入后端

| kind | 是否常驻 | 依赖 |
|---|---|---|
| `http` | 是 | 零依赖，只需 URL |
| `static` | 是 | 纯 Go 查表；模型文件可选（`model_path`） |
| `onnx` | 否 | 需 `-tags onnx` 且 `CGO_ENABLED=1`，链接 libonnxruntime |

`version` 与 `status` 都会打印编译期能力行，例如：
`embed capabilities: http (always) | static (compiled) | onnx (not compiled)`

**降级链硬约束**：同一条链上所有后端必须同模型同维度；
`gate_threshold` 必须按 provider 配，分数区间随模型变化，不可跨模型复用。

---

## 八、平台支持

| 平台 | 状态 | 证据 |
|---|---|---|
| linux amd64/arm64 | 已实现 | CI 交叉编译 + Linux 全量测试 |
| darwin amd64/arm64 | 已实现 | 本地全量测试 + CI 交叉编译 |
| windows amd64/arm64 | 已实现 | CI 在 `windows-latest` 真实跑测试 |

Windows 曾经有四处真实缺陷，由 CI 抓出并修复（2026-11-21）：
路径归一化、密钥/token 权限位判断、本地远端路径识别、实例锁句柄释放。
这类问题只在真平台暴露——**有平台差异就必须在真平台跑测试**。

---

## 九、明确的未实现 / 不做

1. **对象写后回读校验、后台巡边**：设计已写，代码未做。
2. **跨节点 ACK 账本持久化**：当前水位只在进程内。
3. **对外部托管仓库的彻底抹除**：不做，也不承诺。
4. **Agent 直连工作层协议**：不做；节点间协议是服务对服务。
5. **审计账本纳入可改写范围**：不做；世界树可改，账本不可。
6. **`center` / `edge` 角色开关**：曾经实现，已删除——那是错的抽象。
