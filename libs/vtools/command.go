package vtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/veypi/vsh/commands"
)

// maxInputBytes 是 --input 的体积上限（与 mcp call 原约定一致）。
const maxInputBytes = 1 << 20

// reservedFlags 是命令面保留 flag：工具 schema 里的同名属性无法经 flag 传递
// （--json/--input/--image-preview/--help 先被命令面消费），只能走 --input。
var reservedFlags = map[string]bool{"json": true, "input": true, "image-preview": true, "help": true, "h": true}

func aliasUsage(alias string) string {
	return `usage:
  ` + alias + `                          列出该服务的工具目录
  ` + alias + ` <tool> --help            按 inputSchema 渲染参数表
  ` + alias + ` <tool> [--flag value]... [--input JSON|-] [--json] [--image-preview WIDTHxHEIGHT]

标量参数（string/number/integer/boolean）自动映射为同名 flag；布尔可裸写
（--flag = true）；复杂对象/数组走 --input（JSON 对象，flag 覆盖同名键）。
以 -- 开头的值用 --flag=value 形式。--image-preview 仅部分服务支持。`
}

// schemaProp 是 inputSchema 单个属性的最小投影。
type schemaProp struct {
	Type        any    `json:"type"` // string 或 []any（取第一个非 "null"）
	Description string `json:"description"`
	Enum        []any  `json:"enum"`
	Default     any    `json:"default"`
}

type schemaDoc struct {
	Properties map[string]schemaProp `json:"properties"`
	Required   []string              `json:"required"`
}

func parseSchema(raw json.RawMessage) *schemaDoc {
	if len(raw) == 0 {
		return nil
	}
	var doc schemaDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Properties == nil {
		return nil
	}
	return &doc
}

// scalarType 取可映射为 flag 的标量类型；type 数组取第一个非 "null"。
// 非标量（object/array 等）返回 ""。
func scalarType(t any) string {
	one := func(v any) string {
		s, _ := v.(string)
		switch s {
		case "string", "boolean", "integer", "number":
			return s
		}
		return ""
	}
	if list, ok := t.([]any); ok {
		for _, v := range list {
			if s, _ := v.(string); s != "null" {
				return one(v)
			}
		}
		return ""
	}
	return one(t)
}

func schemaTypeText(t any) string {
	if list, ok := t.([]any); ok {
		parts := make([]string, 0, len(list))
		for _, v := range list {
			parts = append(parts, fmt.Sprint(v))
		}
		return strings.Join(parts, "|")
	}
	if t == nil {
		return "any"
	}
	return fmt.Sprint(t)
}

func (d *schemaDoc) required(name string) bool {
	for _, r := range d.Required {
		if r == name {
			return true
		}
	}
	return false
}

// convertFlag 把 flag 原文按 schema 类型转换。explicit 区分「--flag=」
// （显式空值）与裸 flag：string 类型的显式空串合法，裸 flag 仅布尔合法。
func convertFlag(name, raw string, explicit bool, prop schemaProp) (any, error) {
	switch typ := scalarType(prop.Type); typ {
	case "boolean":
		if raw == "" && !explicit {
			return true, nil
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("flag --%s: invalid boolean %q", name, raw)
		}
		return v, nil
	case "integer":
		if raw == "" {
			return nil, fmt.Errorf("flag --%s requires a value", name)
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("flag --%s: invalid integer %q", name, raw)
		}
		return v, nil
	case "number":
		if raw == "" {
			return nil, fmt.Errorf("flag --%s requires a value", name)
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("flag --%s: invalid number %q", name, raw)
		}
		return v, nil
	case "string":
		if raw == "" && !explicit {
			return nil, fmt.Errorf("flag --%s requires a value（空串写 --%s=）", name, name)
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("flag --%s: schema type %s 不能映射为 flag，请走 --input", name, schemaTypeText(prop.Type))
	}
}

// FindTool 按名查找工具（目录/参数表与调试直通共用）。
func FindTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func toolNames(tools []Tool) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// renderToolHelp 按 inputSchema 渲染参数表。
func renderToolHelp(inv *commands.Invocation, alias string, tool Tool) {
	out := inv.Stdout
	fmt.Fprintf(out, "%s %s", alias, tool.Name)
	if tool.Description != "" {
		fmt.Fprintf(out, " — %s", tool.Description)
	}
	fmt.Fprintln(out)
	doc := parseSchema(tool.InputSchema)
	if doc == nil || len(doc.Properties) == 0 {
		fmt.Fprintln(out, "（无参数或 schema 未声明；复杂参数走 --input JSON）")
		return
	}
	names := make([]string, 0, len(doc.Properties))
	for name := range doc.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prop := doc.Properties[name]
		var b strings.Builder
		fmt.Fprintf(&b, "  --%s (%s)", name, schemaTypeText(prop.Type))
		if doc.required(name) {
			b.WriteString(" 必填")
		}
		if reservedFlags[name] {
			b.WriteString("（保留 flag 名，此参数请走 --input）")
		}
		if prop.Default != nil {
			raw, err := json.Marshal(prop.Default)
			if err == nil {
				fmt.Fprintf(&b, " 默认 %s", raw)
			}
		}
		if prop.Description != "" {
			fmt.Fprintf(&b, "\n      %s", prop.Description)
		}
		if len(prop.Enum) > 0 {
			raw, err := json.Marshal(prop.Enum)
			if err == nil {
				fmt.Fprintf(&b, "\n      取值: %s", raw)
			}
		}
		fmt.Fprintln(out, b.String())
	}
}

// RenderCatalog 渲染服务工具目录（<alias> 无参 / --json）。
func RenderCatalog(ctx context.Context, inv *commands.Invocation, alias string, p Provider, jsonOut bool) error {
	tools, err := p.Tools(ctx)
	if err != nil {
		return commands.Exitf(inv, 1, "%s: %s", alias, err)
	}
	if jsonOut {
		return WriteResult(inv, tools, true)
	}
	if len(tools) == 0 {
		fmt.Fprintf(inv.Stdout, "%s: (no tools)\n", alias)
		return nil
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	for _, t := range tools {
		if t.Description != "" {
			fmt.Fprintf(inv.Stdout, "%s\t%s\n", t.Name, t.Description)
		} else {
			fmt.Fprintln(inv.Stdout, t.Name)
		}
	}
	return nil
}

// WriteResult 渲染调用结果：默认缩进 JSON，--json 紧凑；下游提前关闭按
// SIGPIPE 语义返回 141（与 vsh 内建命令同一约定）。
func WriteResult(inv *commands.Invocation, value any, compact bool) error {
	enc := json.NewEncoder(inv.Stdout)
	if !compact {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(value); err != nil {
		if commands.BrokenPipe(err) {
			return &commands.ExitError{Code: 141}
		}
		return err
	}
	return nil
}

// commandFunc 生成服务 alias 的 vsh 根命令：工具目录 / 参数表 / 调用。
func (r *Registry) commandFunc(alias string) commands.CommandFunc {
	return func(ctx context.Context, inv *commands.Invocation) error {
		return r.RunTool(ctx, inv, alias)
	}
}

// RunTool 是已登记服务的工具调用入口（查找 + RunProvider）。
// 命令准入由 vsh 在调用本函数之前统一完成。
func (r *Registry) RunTool(ctx context.Context, inv *commands.Invocation, alias string) error {
	r.mu.RLock()
	e := r.entries[alias]
	r.mu.RUnlock()
	if e == nil {
		return commands.Exitf(inv, 1, "%s: service is not registered", alias)
	}
	return RunProvider(ctx, inv, alias, e.provider)
}

// RunProvider 是工具调用内核（目录/参数表/调用），不依赖登记表；调试直通
// （mcp tools/describe/call）与自定义命令动词分发后直接复用。调用门由调用方
// 负责（Registry.RunTool 或已认证的调试入口）。
func RunProvider(ctx context.Context, inv *commands.Invocation, alias string, p Provider) error {
	var (
		positional   []string
		input        string
		hasInput     bool
		jsonOut      bool
		preview      string
		help         bool
		flagValues   = map[string]string{}
		flagExplicit = map[string]bool{} // 显式给值（--flag=v / --flag v / --flag=）vs 裸 flag
		flagNames    []string
	)
	recordFlag := func(name, value string, explicit bool) error {
		if name == "" {
			return fmt.Errorf("invalid flag")
		}
		if _, dup := flagValues[name]; !dup {
			flagNames = append(flagNames, name)
		}
		flagValues[name] = value
		flagExplicit[name] = explicit
		return nil
	}
	args := inv.Args
	for n := 0; n < len(args); n++ {
		a := args[n]
		switch {
		case a == "--help" || a == "-h":
			help = true
		case a == "--json":
			jsonOut = true
		case a == "--input":
			if n+1 >= len(args) {
				return commands.Exitf(inv, 2, "%s: --input requires a value", alias)
			}
			input, hasInput = args[n+1], true
			n++
		case a == "--image-preview":
			if n+1 >= len(args) {
				return commands.Exitf(inv, 2, "%s: --image-preview requires a value", alias)
			}
			if preview != "" {
				return commands.Exitf(inv, 2, "%s: --image-preview may only be specified once", alias)
			}
			preview = args[n+1]
			n++
		case strings.HasPrefix(a, "--"):
			body := strings.TrimPrefix(a, "--")
			if i := strings.IndexByte(body, '='); i >= 0 {
				if err := recordFlag(body[:i], body[i+1:], true); err != nil {
					return commands.Exitf(inv, 2, "%s: %s", alias, err)
				}
			} else if n+1 < len(args) && !strings.HasPrefix(args[n+1], "--") {
				if err := recordFlag(body, args[n+1], true); err != nil {
					return commands.Exitf(inv, 2, "%s: %s", alias, err)
				}
				n++
			} else {
				if err := recordFlag(body, "", false); err != nil {
					return commands.Exitf(inv, 2, "%s: %s", alias, err)
				}
			}
		default:
			positional = append(positional, a)
		}
	}

	if help && len(positional) == 0 {
		fmt.Fprintln(inv.Stdout, aliasUsage(alias))
		return nil
	}
	if len(positional) == 0 {
		return RenderCatalog(ctx, inv, alias, p, jsonOut)
	}
	tool := positional[0]
	if len(positional) != 1 {
		return commands.Exitf(inv, 2, "%s", aliasUsage(alias))
	}

	// schema 只在需要时取（--help / flag 转换 / 工具名校验）；纯 --input
	// 调用直达，不多花一次 tools/list 往返。
	var (
		schema  *schemaDoc
		toolDef *Tool
	)
	needSchema := help || len(flagNames) > 0
	if needSchema {
		tools, err := p.Tools(ctx)
		if err != nil {
			return commands.Exitf(inv, 1, "%s: %s", alias, err)
		}
		toolDef = FindTool(tools, tool)
		if toolDef == nil {
			return commands.Exitf(inv, 1, "%s: unknown tool %q（可用: %s）", alias, tool, toolNames(tools))
		}
		schema = parseSchema(toolDef.InputSchema)
	}
	if help {
		renderToolHelp(inv, alias, *toolDef)
		return nil
	}

	callArgs := map[string]any{}
	if hasInput {
		var data []byte
		if input == "-" {
			if inv.Stdin == nil {
				return commands.Exitf(inv, 1, "%s: stdin is unavailable", alias)
			}
			var err error
			data, err = io.ReadAll(io.LimitReader(inv.Stdin, maxInputBytes+1))
			if err != nil {
				return commands.Exitf(inv, 1, "%s: %s", alias, err)
			}
		} else {
			data = []byte(input)
		}
		if len(data) > maxInputBytes {
			return commands.Exitf(inv, 2, "%s: input exceeds 1 MiB", alias)
		}
		if err := json.Unmarshal(data, &callArgs); err != nil {
			return commands.Exitf(inv, 2, "%s: --input must be a JSON object: %s", alias, err)
		}
		if callArgs == nil {
			return commands.Exitf(inv, 2, "%s: --input must be a JSON object", alias)
		}
	}
	if len(flagNames) > 0 {
		if schema == nil {
			return commands.Exitf(inv, 2, "%s: tool %q 未声明 inputSchema，参数请走 --input", alias, tool)
		}
		for _, name := range flagNames {
			prop, ok := schema.Properties[name]
			if !ok {
				mappable := make([]string, 0, len(schema.Properties))
				for pname, pprop := range schema.Properties {
					if reservedFlags[pname] {
						continue // 保留名被命令面消费，列出来只会误导
					}
					if scalarType(pprop.Type) != "" {
						mappable = append(mappable, "--"+pname)
					}
				}
				sort.Strings(mappable)
				return commands.Exitf(inv, 2, "%s: unknown flag --%s（可映射: %s；其余走 --input）", alias, name, strings.Join(mappable, " "))
			}
			value, err := convertFlag(name, flagValues[name], flagExplicit[name], prop)
			if err != nil {
				return commands.Exitf(inv, 2, "%s: %s", alias, err)
			}
			callArgs[name] = value
		}
	}

	// --image-preview 的支持性检查必须先于调用：不支持的 Provider 不能先真跑
	// 完工具（副作用已落地）再报错。
	var previewer ImagePreviewer
	if preview != "" {
		pv, ok := p.(ImagePreviewer)
		if !ok {
			return commands.Exitf(inv, 2, "%s: 该服务不支持 --image-preview", alias)
		}
		previewer = pv
	}

	result, err := p.Call(ctx, tool, callArgs)
	if err != nil {
		var callErr *CallError
		if errors.As(err, &callErr) {
			if werr := WriteResult(inv, callErr.Result, jsonOut); werr != nil {
				return werr
			}
			return &commands.ExitError{Code: 1}
		}
		return commands.Exitf(inv, 1, "%s: %s", alias, err)
	}
	if preview != "" {
		if result, err = previewer.PreviewImages(result, preview); err != nil {
			return commands.Exitf(inv, 1, "%s: %s", alias, err)
		}
	}
	return WriteResult(inv, result, jsonOut)
}
