module github.com/open-mcp-ai/zero-memcp

go 1.27.3

require (
	github.com/open-mcp-ai/termcp v0.2.9
	github.com/yalue/onnxruntime_go v1.38.0
	golang.org/x/crypto v0.58.0
	golang.org/x/sys v0.49.0
)

require mvdan.cc/sh/v3 v3.11.0 // indirect

replace github.com/open-mcp-ai/termcp => ../termcp
