// Package auth 负责两件事：权限位的解析，以及凭证的签发与校验。
//
// 它不认识「用户」也不认识「项目」——那些是配置的概念，住在 config 包里。
// 这里只提供两块底座：
//
//   - Perm：九位掩码的解析与格式化（配置文件里写的 0755 / rwxr-x---）
//   - TokenStore：HMAC-SHA256 签名的无状态凭证
//
// 为什么权限判定不在这里：判定需要知道「谁是属主、谁在组里、项目的 mode
// 是什么」，而这些都是配置的函数。放在这里就得把配置反向依赖进 auth，
// 或者让 auth 持有一份它不该知道的索引。
package auth

import (
	"fmt"
	"strconv"
	"strings"
)

// Perm 是九位权限位，按 POSIX 分成三组：属主 / 属组 / 其他。
//
// 它描述的是「一个项目（一个裸仓库）对外怎么开放」，不是文件属性。
// 项目本来就只有一个粒度——整个仓库给你或整个不给你，
// 没有「这个文件能读那个文件不能读」这回事。
type Perm struct {
	Owner uint8 // 0..7
	Group uint8 // 0..7
	Other uint8 // 0..7
}

// 权限位本身。
//
// 语义在记忆场景里的含义：
//
//	r = 读：recall / read / grep / ls
//	w = 写：note / commit（追加到这条 ref）
//	x = 作为规则执行：可以注入系统提示
//
// x 比 r 更强，所以只授 r 的凭证即使能看到内容，也不会被塞进系统提示。
const (
	BitRead    uint8 = 4
	BitWrite   uint8 = 2
	BitExecute uint8 = 1
)

// ParsePerm 解析两种写法：
//
//	rwxr-x---   （九字符）
//	0750 / 750  （八进制）
//
// 前者给人读，后者给 chmod 用。同一个东西的两种拼写，不引入第三种。
func ParsePerm(s string) (Perm, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Perm{}, fmt.Errorf("权限位为空")
	}

	if s[0] == '0' || (len(s) == 3 && isOctal(s)) {
		v, err := strconv.ParseUint(strings.TrimPrefix(s, "0"), 8, 32)
		if err != nil || v > 0777 {
			return Perm{}, fmt.Errorf("非法八进制权限 %q", s)
		}
		return Perm{
			Owner: uint8(v>>6) & 7,
			Group: uint8(v>>3) & 7,
			Other: uint8(v) & 7,
		}, nil
	}

	if len(s) != 9 {
		return Perm{}, fmt.Errorf("权限位必须是九位（rwxr-x---）或八进制（0750），得到 %q", s)
	}
	rwx := func(tri string, at int) (uint8, error) {
		var v uint8
		for i := 0; i < 3; i++ {
			ch := tri[i]
			want := []byte{'r', 'w', 'x'}[i]
			switch {
			case ch == want:
				v |= []uint8{BitRead, BitWrite, BitExecute}[i]
			case ch == '-' || ch == ' ':
				// 没有这位
			default:
				return 0, fmt.Errorf("第 %d 位应是 %c 或 -，得到 %c（整体 %q）", at+i+1, want, ch, s)
			}
		}
		return v, nil
	}
	o, err := rwx(s[0:3], 0)
	if err != nil {
		return Perm{}, err
	}
	g, err := rwx(s[3:6], 3)
	if err != nil {
		return Perm{}, err
	}
	x, err := rwx(s[6:9], 6)
	if err != nil {
		return Perm{}, err
	}
	return Perm{Owner: o, Group: g, Other: x}, nil
}

func isOctal(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// String 拼回九字符，给日志和列表用。
func (p Perm) String() string {
	b := make([]byte, 9)
	tri := func(v uint8, at int) {
		b[at], b[at+1], b[at+2] = '-', '-', '-'
		if v&BitRead != 0 {
			b[at] = 'r'
		}
		if v&BitWrite != 0 {
			b[at+1] = 'w'
		}
		if v&BitExecute != 0 {
			b[at+2] = 'x'
		}
	}
	tri(p.Owner, 0)
	tri(p.Group, 3)
	tri(p.Other, 6)
	return string(b)
}

// Octal 拼成 0750 这种。
func (p Perm) Octal() string {
	return "0" + strconv.FormatUint(uint64(p.Owner)<<6|uint64(p.Group)<<3|uint64(p.Other), 8)
}

// PermName 把单个权限位转成字母，用于错误信息。
func PermName(perm uint8) string {
	switch perm {
	case BitRead:
		return "r"
	case BitWrite:
		return "w"
	case BitExecute:
		return "x"
	}
	return "?"
}
