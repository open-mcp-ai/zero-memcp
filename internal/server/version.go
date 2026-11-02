package server

// Version 是当前二进制版本号。构建时用 -ldflags 注入。
//
// 为什么要能注入而不是写死：容器里排障的第一件事就是
// 「这台跑的到底是哪一版」。写死的常量意味着每次都要重新构建才准。
var Version = "0.1.0-dev"
