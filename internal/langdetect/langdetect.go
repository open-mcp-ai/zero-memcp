package langdetect

import (
	"unicode"
)

// Lang 是一门语言在检索侧的标识。
//
// 只区分「检索时真的会走不同索引」的几类：中文、拉丁（英文为主）、
// 混合、其他。不做语种分类学——把日语和中文分开对检索没有意义，
// 它们该进同一个索引，因为两者的词表高度重叠。
type Lang string

const (
	ZH      Lang = "zh"
	EN      Lang = "en"
	MIXED   Lang = "mixed"
	UNKNOWN Lang = "unknown"
)

// Result 是判定结果，带比例，便于回显与审计。
//
// 为什么要带 zhRatio 而不是只给一个标签：同一句话两次判出不同标签时，
// 只有比例能解释发生了什么。标签是结论，比例是证据。
type Result struct {
	Lang    Lang    `json:"lang"`
	ZHRatio float64 `json:"zh_ratio"`
	// Counted 是被计入判定的字符数（不含空白与标点）。
	Counted int `json:"counted"`
}

// Detect 按脚本字符占比判定文本语言。
//
// 判据故意做得极简：中日韩表意/假名算「中文侧」，拉丁字母算「英文侧」。
// 不从词汇、语法或统计模型切入，理由有三：
//
//	① 判错后果很轻：判错只是先查了错的索引，另一条路仍会兜底；
//	② 可解释：出问题时能指着「12 个汉字 3 个字母」说明白；
//	③ 无依赖：不需要模型文件，边缘小盒子也能跑。
//
// 混合文本（本项目的常态：中文叙述夹英文标识符）判 MIXED，
// 调用方据此两路都查、用排名融合合并——绝不比较两个模型的原始分数。
func Detect(text string) Result {
	var zh, latin int
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r),
			unicode.Is(unicode.Hiragana, r),
			unicode.Is(unicode.Katakana, r),
			unicode.Is(unicode.Hangul, r):
			zh++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			latin++
		}
	}
	counted := zh + latin
	if counted == 0 {
		return Result{Lang: UNKNOWN}
	}
	ratio := float64(zh) / float64(counted)
	res := Result{ZHRatio: ratio, Counted: counted}
	switch {
	case zh == 0:
		res.Lang = EN
	case latin == 0:
		res.Lang = ZH
	case ratio >= 0.75:
		res.Lang = ZH
	case ratio <= 0.25:
		res.Lang = EN
	default:
		res.Lang = MIXED
	}
	return res
}