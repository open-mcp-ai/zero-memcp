# 贡献指南

## 开发环境

```bash
git clone https://github.com/open-mcp-ai/zero-memcp.git
cd zero-memcp
go test ./... -count=1
```

要求 Go 1.24+。**零外部依赖**——所有功能只用标准库实现，不引入第三方模块。

## 提交前必做

```bash
make test          # 8 个包全绿
go vet ./...       # 无警告
gofmt -l .         # 无输出（格式正确）
```

## 代码约定

### 注释写「为什么」，不写「做什么」

```go
// 好：
// WAL 只追加，永不重写。重写 = 写新文件 + rename，
// 中间被杀最坏丢掉整个原日志。

// 差：
// 打开 WAL 文件并追加一行。
```

### 测试必须能在三个平台跑

CI 在 ubuntu / macos / windows × `-race` 下运行。所以：

- 禁止硬编码绝对路径，用 `t.TempDir()`
- 禁止依赖执行顺序，用 `-shuffle=on` 验证
- 并发场景用 `make test-stress`

### 不引入外部依赖

需要什么就自己写。这个项目的价值之一就是「`go build` 不需要联网拉包」。

## 提交信息

```
<类型>: <一句话说明>

<可选：为什么这么改，而不是另一种改法>
```

类型：`feat` / `fix` / `docs` / `test` / `refactor` / `chore`

## 报告问题

用 GitHub Issues。请说明：

1. 你期望发生什么
2. 实际发生了什么
3. 最小复现步骤
