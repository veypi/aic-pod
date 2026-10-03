package policy

import "github.com/veypi/vbox"

// 规则解析与运行时匹配共用 vbox 的目标格式。
type Entry = vbox.Entry

func ParseEntry(s string) (Entry, error)  { return vbox.ParseEntry(s) }
func ValidateEntries(list []string) error { return vbox.ValidateEntries(list) }
