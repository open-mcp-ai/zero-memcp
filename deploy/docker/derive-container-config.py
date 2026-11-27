#!/usr/bin/env python3
"""从宿主机的 zero-memcp.toml 派生容器用配置。

为什么不手写第二份配置：配置会漂。本机加了 remote、加了 push 分支之后，
手抄的那份不会跟着变，然后在某个凌晨表现为「同步静默落后」——那种故障
最难查，因为两边看起来都正常，只是内容不一致。

所以这里做逐行替换：只在命中期望的行时替换，命中次数不是 1 就报错退出。
不做正则替换（会误伤注释里的同名字符串），不吞掉不认识的差异。
"""
import argparse
import os
import sys

# 宿主机一行 -> 容器一行。键必须精确匹配整行（含缩进），
# 这样注释里的同类文本不会被误改。
RULES = [
    (
        'data   = "{old_data}"\n',
        'data   = "/data"\nenforce_auth = true\n',
        "数据目录挂载点 + 非回环监听必须带凭证",
    ),
    (
        'addr  = "127.0.0.1:1729"\n',
        'addr  = "0.0.0.0:1729"\n',
        "容器内必须监听 0.0.0.0，端口转发才通（宿主侧只映射回环）",
    ),
    (
        'allow = ["*"]\n',
        'allow = ["*"]\nrequire = "bearer"\n',
        "端口级凭证要求，与 enforce_auth 等价",
    ),
    (
        'vectors           = "{old_data}/vectors"\n',
        'vectors           = "/data/vectors"\n',
        "向量缓存挂载点",
    ),
    (
        'endpoint = "http://127.0.0.1:11434"\n',
        'endpoint = "http://host.docker.internal:11434"\n',
        "容器里的 127.0.0.1 是容器自己，宿主 ollama 要经 host.docker.internal",
    ),
]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", default=os.path.expanduser("~/.zero-memcp/zero-memcp.toml"))
    ap.add_argument("--dst", default=os.path.expanduser("~/.zero-memcp/zero-memcp.container.toml"))
    args = ap.parse_args()

    raw = open(args.src, "rb").read()
    if raw.startswith(b"\xef\xbb\xbf"):
        print(f"✗ {args.src} 带 BOM，先去掉再派生", file=sys.stderr)
        return 1
    text = raw.decode("utf-8")

    old_data = os.path.abspath(os.path.expanduser("~/.zero-memcp/data"))

    lines = text.splitlines(keepends=True)
    hit = [0] * len(RULES)
    out = []
    for ln in lines:
        replaced = False
        for i, (pat, rep, _why) in enumerate(RULES):
            want = pat.format(old_data=old_data)
            if ln == want:
                hit[i] += 1
                out.append(rep.format(old_data=old_data))
                replaced = True
                break
        if not replaced:
            out.append(ln)

    bad = [(i, RULES[i][2], hit[i]) for i in range(len(RULES)) if hit[i] != 1]
    if bad:
        print("✗ 替换次数不符合预期（每条规则必须精确命中 1 次）：", file=sys.stderr)
        for i, why, n in bad:
            print(f"    命中 {n} 次 -> {RULES[i][0].format(old_data=old_data)!r}  ({why})", file=sys.stderr)
        print("  宿主机配置可能已经变了；请核对后更新本脚本的 RULES，不要手改产物。", file=sys.stderr)
        return 1

    os.makedirs(os.path.dirname(args.dst), exist_ok=True)
    open(args.dst, "wb").write("".join(out).encode("utf-8"))

    if open(args.dst, "rb").read().startswith(b"\xef\xbb\xbf"):
        print("✗ 产物带 BOM", file=sys.stderr)
        return 1

    try:
        import tomllib
        tomllib.loads(open(args.dst).read())
    except Exception as e:  # noqa: BLE001
        print(f"✗ 产物不是合法 TOML: {e}", file=sys.stderr)
        return 1

    print(f"✓ {args.dst}")
    for i, (_p, _r, why) in enumerate(RULES):
        print(f"    {why}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
