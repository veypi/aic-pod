package browser

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// mergeLocks 进程内按目标路径串行化 merge（v0.14.5 §4 评审修复：state 文件为
// 用户级共享单文件，两个 browser 实例并发 save 时 read-modify-write 会互相
// 覆盖丢失站点——原子 rename 只防损坏不防 lost update。cloud/host 均单进程
// 写 state 文件，进程内锁足够）。
var mergeLocks sync.Map // path(string) -> *sync.Mutex

// state 文件按站点 merge（v0.14.5 §4）：agent-browser `state save` 导出当前实例
// 的全量 cookies/storage；跨会话共享一个 browser.json 时，不同 session 访问
// 不同站点不得互覆——merge 键：
//   - cookie：(domain, path, name)，新覆盖旧、旧未涉保留；
//   - storage：(origin, name)，新覆盖旧、旧未涉保留；
//   - 顺带清理过期 cookie（expires < now；expires<=0/缺省 = session cookie 保留）。
//
// schema 以 agent-browser 导出为准（playwright storageState：顶层 cookies +
// origins，实测 0.31.1 恒此形态；旧档的扁平 localStorage/sessionStorage 宽容
// 已按实测裁剪——不存在此形态的产出方）。输出保持「新导出」的形态
//（dst 原有未知顶层字段：新有取新、新无保留旧）。
//
// mergeStateFile 将 src（新导出）merge 进 dst（既有 browser.json，可不存在），
// 原子写回 dst（临时文件 + rename），返回 save 摘要。

// SaveSummary 是 save 摘要（§4：sites/updated/kept 随响应返回）。
type SaveSummary struct {
	Sites   int `json:"sites"`   // 涉及的站点数（cookie domain + storage origin 去重）
	Updated int `json:"updated"` // 本次新写入的条目数（cookies + storage 键）
	Kept    int `json:"kept"`    // 保留的既有条目数（未被本次覆盖）
}

// mergeStateFile 把 src 合并进 dst 并原子写回；dst 不存在时直接落 src
// （仍做过期清理）。任何解析失败按「原样保留 dst、不写回」处理（best-effort，
// state 保存不阻断浏览动作）。
func mergeStateFile(dst, src string, now time.Time) *SaveSummary {
	lock, _ := mergeLocks.LoadOrStore(dst, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	rawNew, err := os.ReadFile(src)
	if err != nil {
		return nil
	}
	var newDoc map[string]any
	if err := json.Unmarshal(rawNew, &newDoc); err != nil {
		return nil
	}

	var oldDoc map[string]any
	if rawOld, err := os.ReadFile(dst); err == nil {
		_ = json.Unmarshal(rawOld, &oldDoc) // 损坏的既有文件 = 视为无旧档
	}
	if oldDoc == nil {
		oldDoc = map[string]any{}
	}

	out := map[string]any{}
	for k, v := range oldDoc {
		out[k] = v
	}
	sum := &SaveSummary{}
	sites := map[string]bool{}

	// cookies：按 (domain,path,name) merge + 过期清理
	newCookies := mergeCookies(toArr(oldDoc["cookies"]), toArr(newDoc["cookies"]), now, sites, sum)
	if newCookies != nil || oldDoc["cookies"] != nil || newDoc["cookies"] != nil {
		out["cookies"] = newCookies
	}

	// storage：playwright origins 形态（agent-browser 导出唯一形态）
	out["origins"] = mergeOrigins(toArr(oldDoc["origins"]), toArr(newDoc["origins"]), sites, sum)
	sum.Sites = len(sites)

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil
	}
	tmp := dst + ".merge-tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return nil
	}
	return sum
}

// mergeCookies 合并 cookie 数组：new 覆盖 old（键 domain|path|name），
// 旧未涉保留；两侧均清理过期项。返回合并后数组（可能为空数组）。
func mergeCookies(oldC, newC []any, now time.Time, sites map[string]bool, sum *SaveSummary) []any {
	type key struct{ d, p, n string }
	oldM := map[key]any{}
	for _, c := range oldC {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		k := key{str(m["domain"]), str(m["path"]), str(m["name"])}
		if cookieExpired(m, now) {
			continue
		}
		oldM[k] = m
	}
	newM := map[key]any{}
	for _, c := range newC {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		k := key{str(m["domain"]), str(m["path"]), str(m["name"])}
		if cookieExpired(m, now) {
			continue
		}
		newM[k] = m
		sites[str(m["domain"])] = true
	}
	// 新覆盖旧；旧未涉保留
	merged := map[key]any{}
	for k, v := range oldM {
		merged[k] = v
	}
	for k, v := range newM {
		merged[k] = v
	}
	sum.Updated += len(newM)
	sum.Kept += len(merged) - len(newM)
	out := make([]any, 0, len(merged))
	for _, v := range merged {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].(map[string]any), out[j].(map[string]any)
		if str(a["domain"]) != str(b["domain"]) {
			return str(a["domain"]) < str(b["domain"])
		}
		return str(a["name"]) < str(b["name"])
	})
	return out
}

// mergeOrigins 合并 playwright 形态 origins：[{origin, localStorage:[{name,value}],
// sessionStorage:[...]}]——(origin,name) 新覆盖旧、旧未涉保留。
func mergeOrigins(oldO, newO []any, sites map[string]bool, sum *SaveSummary) []any {
	type key struct {
		origin string
		kind   string // localStorage | sessionStorage
		name   string
	}
	oldM := map[key]map[string]any{}
	collect := func(list []any, into map[key]map[string]any) int {
		n := 0
		for _, o := range list {
			om, ok := o.(map[string]any)
			if !ok {
				continue
			}
			origin := str(om["origin"])
			for _, kind := range []string{"localStorage", "sessionStorage"} {
				for _, e := range toArr(om[kind]) {
					em, ok := e.(map[string]any)
					if !ok {
						continue
					}
					into[key{origin, kind, str(em["name"])}] = em
					n++
				}
			}
		}
		return n
	}
	collect(oldO, oldM)
	newM := map[key]map[string]any{}
	_ = collect(newO, newM)
	merged := map[key]map[string]any{}
	for k, v := range oldM {
		merged[k] = v
	}
	for k, v := range newM {
		merged[k] = v
		sites[k.origin] = true
	}
	sum.Updated += len(newM)
	sum.Kept += len(merged) - len(newM) // 旧未涉保留

	// 按 origin 重组成数组（保持确定性序）
	byOrigin := map[string]map[string][]any{}
	for k, v := range merged {
		if byOrigin[k.origin] == nil {
			byOrigin[k.origin] = map[string][]any{"localStorage": {}, "sessionStorage": {}}
		}
		byOrigin[k.origin][k.kind] = append(byOrigin[k.origin][k.kind], v)
	}
	origins := make([]string, 0, len(byOrigin))
	for o := range byOrigin {
		origins = append(origins, o)
	}
	sort.Strings(origins)
	out := make([]any, 0, len(origins))
	for _, o := range origins {
		om := map[string]any{"origin": o}
		for _, kind := range []string{"localStorage", "sessionStorage"} {
			entries := byOrigin[o][kind]
			sort.Slice(entries, func(i, j int) bool {
				return str(entries[i].(map[string]any)["name"]) < str(entries[j].(map[string]any)["name"])
			})
			om[kind] = entries
		}
		out = append(out, om)
	}
	return out
}

// cookieExpired 判定过期：expires 为秒级 Unix（>1e12 视为毫秒容错）；
// <=0/缺省 = session cookie（不过期）。sameSite/secure 等字段原样保留。
func cookieExpired(m map[string]any, now time.Time) bool {
	e, ok := num(m["expires"])
	if !ok || e <= 0 {
		return false
	}
	if e > 1e12 {
		e = e / 1000
	}
	return e < float64(now.Unix())
}

func toArr(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}
