package fsauth

import "os"

// existingDirs 过滤空串与不存在的目录（沙箱 bind 要求源存在）。
// 平台无关，三端 CacheRoots 共用（此前三份字节级重复，收一份防漂移）。
func existingDirs(dirs ...string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}
