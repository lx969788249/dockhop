package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HumanBytes 把字节数写成 1.5KB、23MB 这样的形式；整数部分达到两位后不再保留小数。
func HumanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 || v >= 10 {
		return fmt.Sprintf("%d%s", int64(v), units[i])
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + units[i]
}

// Elapsed 把时长写成“3 秒”“1 分 3 秒”“1 小时 2 分 3 秒”。
func Elapsed(d time.Duration) string {
	sec := int64(d / time.Second)
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%d 小时 %d 分 %d 秒", sec/3600, sec%3600/60, sec%60)
	case sec >= 60:
		return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
	default:
		return fmt.Sprintf("%d 秒", sec)
	}
}

// CJKSpaced 用于把英文或数字嵌进中文句子：与英文、数字相邻时加一个空格，
// 与中文或全角符号相邻时不加。
func CJKSpaced(text string) string {
	if text == "" {
		return text
	}
	r := []rune(text)
	left, right := "", ""
	if isASCIIAlnum(r[0]) {
		left = " "
	}
	if isASCIIAlnum(r[len(r)-1]) {
		right = " "
	}
	return left + text + right
}

func isASCIIAlnum(c rune) bool {
	return c < 128 && (c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z')
}

// displayWidth 估算文字在终端里占的列数：中文等全角字符占两列。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x2E80 {
			w += 2
		} else {
			w++
		}
	}
	return w
}
