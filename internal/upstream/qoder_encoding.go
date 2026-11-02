// qoder_encoding.go Qoder 换表 Base64 编码：base64 + 自定义字母表 + 三段重排。
// 移植自 qoderwork2api（算法来自其 docs/api-reference.md §5）。
package upstream

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCustomPad      = '$'
)

var (
	qoderStdToCustom [128]byte
	qoderCustomToStd [256]byte
)

func init() {
	for i := range qoderStdToCustom {
		qoderStdToCustom[i] = 0xFF
	}
	for i := range qoderCustomToStd {
		qoderCustomToStd[i] = 0xFF
	}
	for i := 0; i < len(qoderStdAlphabet); i++ {
		qoderStdToCustom[qoderStdAlphabet[i]] = qoderCustomAlphabet[i]
		qoderCustomToStd[qoderCustomAlphabet[i]] = qoderStdAlphabet[i]
	}
	qoderCustomToStd[qoderCustomPad] = '='
}

// QoderEncode 编码：base64 → 三段重排 → 字符映射（'=' → '$'）。
func QoderEncode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	if n == 0 {
		return ""
	}
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if c == '=' {
			sb.WriteByte(qoderCustomPad)
		} else {
			sb.WriteByte(qoderStdToCustom[c])
		}
	}
	return sb.String()
}

// QoderDecode 逆编码：字符映射（'$' → '='）→ 三段逆重排 → base64 解码。
func QoderDecode(enc string) ([]byte, error) {
	n := len(enc)
	if n == 0 {
		return nil, nil
	}
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		c := enc[i]
		s := qoderCustomToStd[c]
		if s == 0xFF {
			return nil, fmt.Errorf("invalid char %q at %d", c, i)
		}
		mapped[i] = s
	}
	a := n / 3
	std := string(mapped[n-a:]) + string(mapped[a:n-a]) + string(mapped[:a])
	return base64.StdEncoding.DecodeString(std)
}
