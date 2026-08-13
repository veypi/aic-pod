package vcore

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- json（§5.4 扩展：JSON 文件工具，view/set/del/append/merge）----
//
// 无外部依赖的内存实现（vcore 包内注册，host/cloud 两端经 vcore.Run 执行）：
//   - view：默认输出结构骨架（不输出值，大文件 token 可控）；--key 提取子树；
//   - set/del/append/merge：读→改→一次原子写（WriteFile 整文件）；
//   - 权限分级见 levels.go jsonSubLevels（view=Read，修改类=Write）。
//
// key 路径：点分段 a.b.c，数组下标 [N] 或 .N（段为数字且当前节点是数组 → 下标，
// 是对象 → 键）。

const (
	// jsonMaxBytes 是整读解析上限（64MB；大文件用 view 结构骨架/--key，输出
	// 永远只有结构/子路径/截断值，不会整文件灌给 AI）。
	jsonMaxBytes = 64 << 20
	// jsonViewDefaultDepth / jsonViewMaxDepth 是结构骨架深度限制。
	jsonViewDefaultDepth = 4
	jsonViewMaxDepth     = 10
	// jsonValueMaxLen 是结构骨架 --values 的标量值截断长度。
	jsonValueMaxLen = 200
)

func init() {
	_ = Register("json", cmdJSON)
}

func cmdJSON(ctx context.Context, env *Env, argv []string) (*Result, error) {
	if len(argv) == 0 {
		return nil, execErr("json", "missing subcommand (view|set|del|append|merge)")
	}
	if argv[0] == "--help" || argv[0] == "-h" || argv[0] == "help" {
		if m, ok := Meta("json"); ok {
			return &Result{Content: m.Help, Attrs: map[string]string{"action": "json"}}, nil
		}
	}
	sub := argv[0]
	rest := argv[1:]
	// 子命令级 --help（`json <sub> --help`）：返回该子命令的文档。
	for _, a := range rest {
		if a == "--help" || a == "-h" {
			if h, ok := jsonSubHelp[sub]; ok {
				return &Result{Content: h, Attrs: map[string]string{"action": "json"}}, nil
			}
			break
		}
	}
	switch sub {
	case "view":
		return jsonView(env, rest)
	case "set":
		return jsonSet(env, rest)
	case "del":
		return jsonDel(env, rest)
	case "append":
		return jsonAppend(env, rest)
	case "merge":
		return jsonMerge(env, rest)
	}
	return nil, execErr("json", "unknown subcommand %q (view|set|del|append|merge)", sub)
}

// ---- 读写辅助 ----

// jsonRead 整读并解析 JSON 文件（64MB 上限）。返回原始字节与解析值。
func jsonRead(env *Env, path string) ([]byte, any, error) {
	abs, err := env.Resolve(path)
	if err != nil {
		return nil, nil, err
	}
	if err := env.CheckPath("json", abs); err != nil {
		return nil, nil, err
	}
	raw, err := env.VFS.ReadFile(abs)
	if err != nil {
		return nil, nil, execErr("json", "%s: %v", path, err)
	}
	if len(raw) > jsonMaxBytes {
		return nil, nil, execErr("json", "%s: file too large (max %dMB)", path, jsonMaxBytes>>20)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, nil, execErr("json", "%s: not valid JSON: %v", path, err)
	}
	return raw, v, nil
}

// jsonWrite 序列化（2 空格缩进 + 结尾换行）并整文件写入。返回写后字节数。
func jsonWrite(env *Env, path string, v any) (int, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return 0, execErr("json", "marshal: %v", err)
	}
	data = append(data, '\n')
	abs, err := env.Resolve(path)
	if err != nil {
		return 0, err
	}
	if err := env.CheckPath("json", abs); err != nil {
		return 0, err
	}
	if err := env.VFS.WriteFile(abs, data, 0o644); err != nil {
		return 0, execErr("json", "%s: %v", path, err)
	}
	return len(data), nil
}

func jsonOK(path, op string, bytes int) *Result {
	summary, _ := json.Marshal(map[string]any{
		"ok": true, "path": path, "ops": []string{op}, "bytes": bytes,
	})
	return &Result{Content: string(summary), Attrs: map[string]string{"action": "json", "path": path}}
}

// ---- key 路径（点分 + [N] 下标）----

type jsonKeySeg struct {
	Key     string // 对象键
	Index   int    // 数组下标（IsIndex 时）
	IsIndex bool   // a.b[0] 显式下标形态
}

var jsonBracketRe = regexp.MustCompile(`^(.+)\[(\d+)\]$`)

// jsonParseKey 解析点路径：a.b.c / a.b[0].c / a.b.0.c。
// 纯数字段不预判语义——取值/设置时按当前节点类型解释（数组 → 下标，对象 → 键）。
func jsonParseKey(path string) ([]jsonKeySeg, error) {
	if strings.TrimSpace(path) == "" {
		return nil, execErr("json", "empty key path")
	}
	parts := strings.Split(path, ".")
	segs := make([]jsonKeySeg, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, execErr("json", "invalid key path %q", path)
		}
		if m := jsonBracketRe.FindStringSubmatch(p); m != nil {
			idx, _ := strconv.Atoi(m[2])
			segs = append(segs, jsonKeySeg{Key: m[1], Index: idx, IsIndex: true})
			continue
		}
		segs = append(segs, jsonKeySeg{Key: p})
	}
	return segs, nil
}

// jsonSegIndex 把段解析为数组下标：显式 [N] 或纯数字键；越界/非数字报错。
func jsonSegIndex(arr []any, s jsonKeySeg) (int, error) {
	var idx int
	if s.IsIndex {
		idx = s.Index
	} else {
		n, err := strconv.Atoi(s.Key)
		if err != nil {
			return 0, execErr("json", "key %q: not found in array", s.Key)
		}
		idx = n
	}
	if idx < 0 || idx >= len(arr) {
		return 0, execErr("json", "index %d out of range (array len %d)", idx, len(arr))
	}
	return idx, nil
}

// jsonGetAt 按路径取值（缺失 → 错误）。
func jsonGetAt(v any, segs []jsonKeySeg) (any, error) {
	cur := v
	for _, s := range segs {
		if s.IsIndex {
			// key[N]：map 取键（必须存在），再按数组下标定位
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, execErr("json", "key %q: parent is not an object", s.Key)
			}
			val, ok := obj[s.Key]
			if !ok {
				return nil, execErr("json", "key %q not found", s.Key)
			}
			arr, ok := val.([]any)
			if !ok {
				return nil, execErr("json", "key %q: parent is not an array", s.Key)
			}
			if s.Index < 0 || s.Index >= len(arr) {
				return nil, execErr("json", "index %d out of range (array len %d)", s.Index, len(arr))
			}
			cur = arr[s.Index]
			continue
		}
		switch node := cur.(type) {
		case map[string]any:
			val, ok := node[s.Key]
			if !ok {
				return nil, execErr("json", "key %q not found", s.Key)
			}
			cur = val
		case []any:
			idx, err := jsonSegIndex(node, s)
			if err != nil {
				return nil, err
			}
			cur = node[idx]
		default:
			return nil, execErr("json", "key %q: parent is not an object or array", s.Key)
		}
	}
	return cur, nil
}

// jsonSetAt 递归设置路径值：中间容器（map 键）不存在自动创建为对象；
// 数组下标越界/非数字报错。返回修改后的文档（数组重建时父容器须写回）。
func jsonSetAt(v any, segs []jsonKeySeg, val any) (any, error) {
	if len(segs) == 0 {
		return val, nil
	}
	s := segs[0]
	last := len(segs) == 1
	if s.IsIndex {
		// key[N]：map 取键（必须存在），再按数组下标定位
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, execErr("json", "key %q: parent is not an object", s.Key)
		}
		child, ok := obj[s.Key]
		if !ok || child == nil {
			return nil, execErr("json", "key %q not found", s.Key)
		}
		arr, ok := child.([]any)
		if !ok {
			return nil, execErr("json", "key %q: parent is not an array", s.Key)
		}
		if s.Index < 0 || s.Index >= len(arr) {
			return nil, execErr("json", "index %d out of range (array len %d)", s.Index, len(arr))
		}
		if last {
			arr[s.Index] = val
			return v, nil
		}
		nc, err := jsonSetAt(arr[s.Index], segs[1:], val)
		if err != nil {
			return nil, err
		}
		arr[s.Index] = nc
		return v, nil
	}
	switch node := v.(type) {
	case map[string]any:
		if last {
			node[s.Key] = val
			return v, nil
		}
		child, ok := node[s.Key]
		if !ok || child == nil {
			child = map[string]any{}
		}
		nc, err := jsonSetAt(child, segs[1:], val)
		if err != nil {
			return nil, err
		}
		node[s.Key] = nc
		return v, nil
	case []any:
		idx, err := jsonSegIndex(node, s)
		if err != nil {
			return nil, err
		}
		if last {
			node[idx] = val
			return v, nil
		}
		nc, err := jsonSetAt(node[idx], segs[1:], val)
		if err != nil {
			return nil, err
		}
		node[idx] = nc
		return v, nil
	case nil:
		if last {
			return val, nil
		}
		return jsonSetAt(map[string]any{}, segs, val)
	default:
		return nil, execErr("json", "key %q: cannot set into non-object/array value", s.Key)
	}
}

// jsonDelAt 递归删除路径值：map 键删除；数组元素删除（重建 slice 返回新值）。
func jsonDelAt(v any, segs []jsonKeySeg) (any, error) {
	if len(segs) == 0 {
		return nil, execErr("json", "empty key path")
	}
	s := segs[0]
	last := len(segs) == 1
	if s.IsIndex {
		// key[N]：map 取键，数组元素删除（重建 slice 写回）
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, execErr("json", "key %q: parent is not an object", s.Key)
		}
		child, ok := obj[s.Key]
		if !ok {
			return nil, execErr("json", "key %q not found", s.Key)
		}
		arr, ok := child.([]any)
		if !ok {
			return nil, execErr("json", "key %q: parent is not an array", s.Key)
		}
		if s.Index < 0 || s.Index >= len(arr) {
			return nil, execErr("json", "index %d out of range (array len %d)", s.Index, len(arr))
		}
		if last {
			obj[s.Key] = append(arr[:s.Index], arr[s.Index+1:]...)
			return v, nil
		}
		nc, err := jsonDelAt(arr[s.Index], segs[1:])
		if err != nil {
			return nil, err
		}
		arr[s.Index] = nc
		return v, nil
	}
	switch node := v.(type) {
	case map[string]any:
		if last {
			if _, ok := node[s.Key]; !ok {
				return nil, execErr("json", "key %q not found", s.Key)
			}
			delete(node, s.Key)
			return v, nil
		}
		child, ok := node[s.Key]
		if !ok {
			return nil, execErr("json", "key %q not found", s.Key)
		}
		nc, err := jsonDelAt(child, segs[1:])
		if err != nil {
			return nil, err
		}
		node[s.Key] = nc
		return v, nil
	case []any:
		idx, err := jsonSegIndex(node, s)
		if err != nil {
			return nil, err
		}
		if last {
			return append(node[:idx], node[idx+1:]...), nil
		}
		nc, err := jsonDelAt(node[idx], segs[1:])
		if err != nil {
			return nil, err
		}
		node[idx] = nc
		return v, nil
	default:
		return nil, execErr("json", "key %q: cannot delete from non-object/array value", s.Key)
	}
}

// jsonAppendAt 递归定位目标数组并追加：中间键不存在 → 末段创建 [val]，
// 中间段创建对象容器；目标非数组报错。
func jsonAppendAt(v any, segs []jsonKeySeg, val any) (any, error) {
	if len(segs) == 0 {
		arr, ok := v.([]any)
		if !ok {
			return nil, execErr("json", "append: target is not an array")
		}
		return append(arr, val), nil
	}
	s := segs[0]
	last := len(segs) == 1
	if s.IsIndex {
		// key[N]：map 取键（必须存在），定位到数组元素后继续递归
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, execErr("json", "key %q: parent is not an object", s.Key)
		}
		child, ok := obj[s.Key]
		if !ok || child == nil {
			return nil, execErr("json", "key %q not found", s.Key)
		}
		arr, ok := child.([]any)
		if !ok {
			return nil, execErr("json", "key %q: parent is not an array", s.Key)
		}
		if s.Index < 0 || s.Index >= len(arr) {
			return nil, execErr("json", "index %d out of range (array len %d)", s.Index, len(arr))
		}
		nc, err := jsonAppendAt(arr[s.Index], segs[1:], val)
		if err != nil {
			return nil, err
		}
		arr[s.Index] = nc
		return v, nil
	}
	switch node := v.(type) {
	case map[string]any:
		child, ok := node[s.Key]
		if !ok || child == nil {
			if last {
				node[s.Key] = []any{val}
				return v, nil
			}
			child = map[string]any{}
		}
		nc, err := jsonAppendAt(child, segs[1:], val)
		if err != nil {
			return nil, err
		}
		node[s.Key] = nc
		return v, nil
	case []any:
		idx, err := jsonSegIndex(node, s)
		if err != nil {
			return nil, err
		}
		nc, err := jsonAppendAt(node[idx], segs[1:], val)
		if err != nil {
			return nil, err
		}
		node[idx] = nc
		return v, nil
	case nil:
		if last {
			return []any{val}, nil
		}
		return jsonAppendAt(map[string]any{}, segs, val)
	default:
		return nil, execErr("json", "key %q: cannot append into non-object/array value", s.Key)
	}
}

// ---- 子命令：view ----

func jsonView(env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("json view", argvSpec{
		bools:  map[string]bool{"--raw": true, "--compact": true, "--values": true},
		values: map[string]bool{"--key": true, "--depth": true},
		minPos: 1, maxPos: 1,
	}, argv)
	if err != nil {
		return nil, err
	}
	p := pa.pos[0]
	if pa.bools["--raw"] && pa.values["--key"] != "" {
		return nil, execErr("json view", "--raw cannot be combined with --key")
	}
	raw, doc, err := jsonRead(env, p)
	if err != nil {
		return nil, err
	}
	if pa.bools["--raw"] {
		content, _ := truncateContent(string(raw), MaxContentBytes)
		return &Result{Content: content, Attrs: map[string]string{"action": "json", "path": p}}, nil
	}
	if k := pa.values["--key"]; k != "" {
		segs, err := jsonParseKey(k)
		if err != nil {
			return nil, err
		}
		sub, err := jsonGetAt(doc, segs)
		if err != nil {
			return nil, err
		}
		var data []byte
		if pa.bools["--compact"] {
			data, err = json.Marshal(sub)
		} else {
			data, err = json.MarshalIndent(sub, "", "  ")
		}
		if err != nil {
			return nil, execErr("json view", "marshal: %v", err)
		}
		content, _ := truncateContent(string(data), MaxContentBytes)
		return &Result{Content: content, Attrs: map[string]string{"action": "json", "path": p}}, nil
	}
	depth := jsonViewDefaultDepth
	if v := pa.values["--depth"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, execErr("json view", "--depth must be >= 1, got %s", v)
		}
		if n > jsonViewMaxDepth {
			n = jsonViewMaxDepth
		}
		depth = n
	}
	abs, err := env.Resolve(p)
	if err != nil {
		return nil, err
	}
	content := abs + ": " + jsonSkeletonLine(doc, 0, depth, pa.bools["--values"])
	return &Result{Content: content, Attrs: map[string]string{"action": "json", "path": p}}, nil
}

// jsonSkeletonLine 渲染结构骨架行（递归展开 object；array 只显示长度；
// 标量显示类型，--values 时附值截断）。
func jsonSkeletonLine(v any, depth, maxDepth int, showValues bool) string {
	switch node := v.(type) {
	case map[string]any:
		var sb strings.Builder
		fmt.Fprintf(&sb, "object (%d keys)", len(node))
		if depth < maxDepth {
			keys := make([]string, 0, len(node))
			for k := range node {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				sb.WriteString("\n")
				sb.WriteString(strings.Repeat("  ", depth+1))
				sb.WriteString(k)
				sb.WriteString(": ")
				sb.WriteString(jsonSkeletonLine(node[k], depth+1, maxDepth, showValues))
			}
		}
		return sb.String()
	case []any:
		return fmt.Sprintf("array[%d]", len(node))
	case string:
		n := utf8.RuneCountInString(node)
		if showValues {
			return fmt.Sprintf("string(%d) %s", n, truncateJSON(node, jsonValueMaxLen))
		}
		return fmt.Sprintf("string(%d)", n)
	case float64:
		if showValues {
			enc, _ := json.Marshal(node)
			return "number " + string(enc)
		}
		return "number"
	case bool:
		if showValues {
			return fmt.Sprintf("bool %t", node)
		}
		return "bool"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// truncateJSON 编码为 JSON 字符串并按字节截断（不切断多字节字符）。
func truncateJSON(s string, maxBytes int) string {
	enc, _ := json.Marshal(s)
	if len(enc) <= maxBytes {
		return string(enc)
	}
	cut := maxBytes
	for cut > 0 && !utf8.Valid(enc[:cut]) {
		cut--
	}
	return string(enc[:cut]) + "..."
}

// ---- 子命令：set / del / append / merge ----

func jsonSet(env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("json set", argvSpec{minPos: 3, maxPos: 3}, argv)
	if err != nil {
		return nil, err
	}
	p, key, raw := pa.pos[0], pa.pos[1], pa.pos[2]
	segs, err := jsonParseKey(key)
	if err != nil {
		return nil, err
	}
	val, err := jsonParseLiteral(raw)
	if err != nil {
		return nil, err
	}
	_, doc, err := jsonRead(env, p)
	if err != nil {
		return nil, err
	}
	nd, err := jsonSetAt(doc, segs, val)
	if err != nil {
		return nil, err
	}
	n, err := jsonWrite(env, p, nd)
	if err != nil {
		return nil, err
	}
	return jsonOK(p, "set "+key, n), nil
}

func jsonDel(env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("json del", argvSpec{minPos: 2, maxPos: 2}, argv)
	if err != nil {
		return nil, err
	}
	p, key := pa.pos[0], pa.pos[1]
	segs, err := jsonParseKey(key)
	if err != nil {
		return nil, err
	}
	_, doc, err := jsonRead(env, p)
	if err != nil {
		return nil, err
	}
	nd, err := jsonDelAt(doc, segs)
	if err != nil {
		return nil, err
	}
	n, err := jsonWrite(env, p, nd)
	if err != nil {
		return nil, err
	}
	return jsonOK(p, "del "+key, n), nil
}

func jsonAppend(env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("json append", argvSpec{minPos: 3, maxPos: 3}, argv)
	if err != nil {
		return nil, err
	}
	p, key, raw := pa.pos[0], pa.pos[1], pa.pos[2]
	segs, err := jsonParseKey(key)
	if err != nil {
		return nil, err
	}
	val, err := jsonParseLiteral(raw)
	if err != nil {
		return nil, err
	}
	_, doc, err := jsonRead(env, p)
	if err != nil {
		return nil, err
	}
	nd, err := jsonAppendAt(doc, segs, val)
	if err != nil {
		return nil, err
	}
	n, err := jsonWrite(env, p, nd)
	if err != nil {
		return nil, err
	}
	return jsonOK(p, "append "+key, n), nil
}

func jsonMerge(env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("json merge", argvSpec{minPos: 2, maxPos: 2}, argv)
	if err != nil {
		return nil, err
	}
	p, raw := pa.pos[0], pa.pos[1]
	val, err := jsonParseLiteral(raw)
	if err != nil {
		return nil, err
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return nil, execErr("json merge", "merge value must be a JSON object")
	}
	_, doc, err := jsonRead(env, p)
	if err != nil {
		return nil, err
	}
	docObj, ok := doc.(map[string]any)
	if !ok {
		return nil, execErr("json merge", "%s: document must be a JSON object (Object.assign semantics)", p)
	}
	for k, v := range obj {
		docObj[k] = v
	}
	n, err := jsonWrite(env, p, docObj)
	if err != nil {
		return nil, err
	}
	return jsonOK(p, "merge", n), nil
}

// jsonParseLiteral 解析 set/append/merge 的值：JSON 字面量（true/false/null/
// 数字/对象/数组/带引号字符串）按 JSON 解析，其余原样按字符串。
var jsonNumberRe = regexp.MustCompile(`^-?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?$`)

func jsonParseLiteral(s string) (any, error) {
	t := strings.TrimSpace(s)
	switch t {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") || strings.HasPrefix(t, `"`) {
		var v any
		if err := json.Unmarshal([]byte(t), &v); err != nil {
			return nil, execErr("json", "invalid JSON literal %q: %v", s, err)
		}
		return v, nil
	}
	if jsonNumberRe.MatchString(t) {
		var v float64
		if err := json.Unmarshal([]byte(t), &v); err == nil {
			return v, nil
		}
	}
	return s, nil
}
