#!/bin/bash
# 下载 zero-memcp 本地语义检索所需的 ONNX 运行时与模型资产。
# 零外部依赖：单二进制通过 CGO 动态加载 libonnxruntime，
# 模型全部在进程内运行，数据不离机。
set -e

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$DIR")"
ASSETS="$ROOT/assets"
mkdir -p "$DIR" "$ASSETS"

MIRROR="${HF_MIRROR:-https://hf-mirror.com}"

echo "=== 1. ONNX Runtime 动态库 ==="
# 优先从本机的 python onnxruntime 提取（若存在），否则从 GitHub Releases 下载
if [ ! -f "$ASSETS/libonnxruntime.1.31.1.dylib" ]; then
  SITE_PKG=$(python3 -c "import site; print(site.getsitepackages()[0])" 2>/dev/null || true)
  CANDIDATE="$SITE_PKG/onnxruntime/capi/libonnxruntime.1.31.1.dylib"
  if [ -f "$CANDIDATE" ]; then
    echo "从本地 Python 环境提取: $CANDIDATE"
    cp "$CANDIDATE" "$ASSETS/"
  else
    echo "下载 libonnxruntime..."
    curl -sL --retry 3 -o "$ASSETS/libonnxruntime.1.31.1.dylib" \
      "https://github.com/microsoft/onnxruntime/releases/download/v1.31.1/onnxruntime-osx-arm64-1.31.1.dylib"
  fi
fi
ls -lh "$ASSETS/libonnxruntime.1.31.1.dylib"

echo
echo "=== 2. LaBSE-int8 模型（跨语言意图，449MB）==="
if [ ! -f "$DIR/labse-int8.onnx" ]; then
  curl -sL --retry 3 -o "$DIR/labse-int8.onnx" \
    "$MIRROR/Xenova/LaBSE/resolve/main/onnx/model_int8.onnx"
fi
if [ ! -f "$DIR/labse-vocab.txt" ]; then
  curl -sL --retry 3 -o "$DIR/labse-vocab.txt" \
    "$MIRROR/Xenova/LaBSE/resolve/main/vocab.txt"
fi
ls -lh "$DIR"/labse-*

echo
echo "=== 资产就绪 ==="
