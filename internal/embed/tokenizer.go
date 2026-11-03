package embed

import (
	"os"
	"strings"
	"unicode"
)

// BertTokenizer: minimal pure-Go WordPiece tokenizer for BERT-family models (bge-*-zh).
type BertTokenizer struct {
	vocab  map[string]int32
	maxLen int
}

const (
	clsID int32 = 101
	sepID int32 = 102
	unkID int32 = 100
	padID int32 = 0
)

func NewBertTokenizer(vocabPath string, maxLen int) (*BertTokenizer, error) {
	b, err := os.ReadFile(vocabPath)
	if err != nil {
		return nil, err
	}
	t := &BertTokenizer{vocab: make(map[string]int32, 32768), maxLen: maxLen}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		tok := strings.TrimRight(line, "\r")
		if tok == "" {
			continue
		}
		t.vocab[tok] = int32(i)
	}
	return t, nil
}

func isPunct(r rune) bool {
	return (r >= 33 && r <= 47) || (r >= 58 && r <= 64) ||
		(r >= 91 && r <= 96) || (r >= 123 && r <= 126)
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

func isSpace(r rune) bool {
	return unicode.IsSpace(r)
}

func isCtrl(r rune) bool {
	return r == 0 || r == 0xFFFD || (r < 32 && !isSpace(r))
}

// basicTokenize implements BERT basic tokenization (CJK split + punctuation split).
func (t *BertTokenizer) basicTokenize(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		switch {
		case isCtrl(r):
			continue
		case isSpace(r):
			flush()
		case isPunct(r):
			flush()
			out = append(out, string(r))
		case isCJK(r):
			flush()
			out = append(out, string(r))
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

func (t *BertTokenizer) wordPiece(word string) []string {
	runes := []rune(word)
	if len(runes) > 100 {
		return []string{unkToken}
	}
	var pieces []string
	start := 0
	for start < len(runes) {
		end := len(runes)
		found := ""
		for start < end {
			sub := string(runes[start:end])
			if start > 0 {
				sub = "##" + sub
			}
			if _, ok := t.vocab[sub]; ok {
				found = sub
				break
			}
			end--
		}
		if found == "" {
			return []string{unkToken}
		}
		pieces = append(pieces, found)
		start = end
	}
	return pieces
}

const unkToken = "[UNK]"

// Encode returns input_ids, attention_mask, token_type_ids (each maxLen long, padded).
func (t *BertTokenizer) Encode(text string) (ids, mask, types []int64) {
	ids = make([]int64, t.maxLen)
	mask = make([]int64, t.maxLen)
	types = make([]int64, t.maxLen)

	toks := []int32{clsID}
	for _, w := range t.basicTokenize(text) {
		for _, p := range t.wordPiece(w) {
			id, ok := t.vocab[p]
			if !ok {
				id = unkID
			}
			toks = append(toks, id)
			if len(toks) >= t.maxLen-1 {
				break
			}
		}
		if len(toks) >= t.maxLen-1 {
			break
		}
	}
	toks = append(toks, sepID)

	for i := range ids {
		if i < len(toks) {
			ids[i] = int64(toks[i])
			mask[i] = 1
		} else {
			ids[i] = int64(padID)
			mask[i] = 0
		}
	}
	return ids, mask, types
}
