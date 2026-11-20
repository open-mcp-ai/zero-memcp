package langdetect

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		in   string
		want Lang
	}{
		{"纯中文句子没有英文字母", ZH},
		{"Hello world, this is a plain English query.", EN},
		{"git commit SHA 4b825dc642cb6eb9a060e54bf8d69288fbee4904 is clean", EN},
		// 中文叙述夹英文专名：7 汉字 + 9 字母，判 mixed 才诚实。
		// mixed 不是「判不出来」，它是明确结论：两路都查、排名融合。
		{"排查 Ollama 的 GPU 占用异常", MIXED},
		// 中文夹大量代码标识符
		{"请修复 writeObject 里的 Stat 盲信点与 applyWorkBlob 校验", MIXED},
		// 纯符号/数字
		{"12345 !@#$", UNKNOWN},
	}
	for _, c := range cases {
		got := Detect(c.in)
		if got.Lang != c.want {
			t.Errorf("Detect(%q): got %s (ratio=%.2f, counted=%d), want %s",
				c.in, got.Lang, got.ZHRatio, got.Counted, c.want)
		}
	}
}
