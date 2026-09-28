// Package cliargs 把 CLI 参数（位置参数 + --flag）解析进结构体
// （browser/cua 的 vsh 指令参数层；协议层无逐指令 argv 调用——
// 这只是 vsh 内部的参数解析实现细节）。
//
// 规则：
//   - 位置参数按声明顺序赋值（positionals 为字段 json 名）；
//   - --name value / --name=value；bool 旗标 --name（=true）/ --name=false；
//   - -- 之后全部按位置参数；
//   - 嵌套结构体一层展平：--ref 命中 locator.ref（顶层无同名字段时）；
//     碰撞时嵌套字段需写全 locator.ref；
//   - []string / []float64 逗号分隔；any 先按 JSON 解析、失败按字符串；
//   - 未知 flag、缺值、类型错误、缺位置参数一律报错（不含糊）。
package cliargs

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// field 是一个可赋值字段的寻址（支持一层嵌套）。
type field struct {
	index  []int
	name   string
	kind   reflect.Kind
	isAny  bool
	isList bool
	elem   reflect.Type
}

// fieldsOf 收集结构体字段（一层嵌套展平；json:"-" 跳过）。
// 嵌套结构体字段以 parent.name 登记；无同名碰撞时同时登记简写 name。
func fieldsOf(t reflect.Type) map[string]field {
	out := map[string]field{}
	type nestedStruct struct {
		prefix []int
		name   string
		rt     reflect.Type
	}
	var nested []nestedStruct
	fieldOf := func(idx []int, name string, ft reflect.Type) field {
		fl := field{index: idx, name: name, kind: ft.Kind(), isAny: ft.Kind() == reflect.Interface}
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.Uint8 {
			fl.isList = true
			fl.elem = ft.Elem()
		}
		return fl
	}
	// 第一趟：顶层字段
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if f.Type.Kind() == reflect.Struct {
			nested = append(nested, nestedStruct{prefix: []int{i}, name: name, rt: f.Type})
			continue
		}
		out[name] = fieldOf([]int{i}, name, f.Type)
	}
	// 第二趟：嵌套结构体（parent.name 恒登记；简写仅无碰撞时）
	for _, n := range nested {
		for i := 0; i < n.rt.NumField(); i++ {
			f := n.rt.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			idx := append(append([]int{}, n.prefix...), i)
			fl := fieldOf(idx, name, f.Type)
			out[n.name+"."+name] = fl
			if _, clash := out[name]; !clash {
				out[name] = fl
			}
		}
	}
	return out
}

// Parse 解析 args 进 out（结构体指针）。positionals 按序声明位置参数字段名；
// 名称带 "?" 后缀 = 可选位置参数（只可出现在末段；缺失不报错）。
func Parse(args []string, positionals []string, out any) error {
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("cliargs: out must be a pointer to struct")
	}
	fields := fieldsOf(v.Elem().Type())
	pos := 0
	posName := func(i int) string { return strings.TrimSuffix(positionals[i], "?") }
	setPos := func(value string) error {
		if pos >= len(positionals) {
			return fmt.Errorf("unexpected positional argument %q", value)
		}
		name := posName(pos)
		pos++
		fl, ok := fields[name]
		if !ok {
			return fmt.Errorf("internal: unknown positional field %q", name)
		}
		return assign(v.Elem(), fl, value)
	}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "--") || a == "--" {
			if err := setPos(a); err != nil {
				return err
			}
			continue
		}
		name := strings.TrimPrefix(a, "--")
		value := ""
		hasValue := false
		if j := strings.IndexByte(name, '='); j >= 0 {
			value, name, hasValue = name[j+1:], name[:j], true
		}
		fl, ok := fields[name]
		if !ok {
			return fmt.Errorf("unknown flag --%s", name)
		}
		if fl.kind == reflect.Bool {
			if !hasValue {
				value = "true"
			}
		} else if !hasValue {
			if i+1 >= len(args) {
				return fmt.Errorf("flag --%s requires a value", name)
			}
			i++
			value = args[i]
		}
		if err := assign(v.Elem(), fl, value); err != nil {
			return fmt.Errorf("flag --%s: %w", name, err)
		}
	}
	for ; i < len(args); i++ {
		if err := setPos(args[i]); err != nil {
			return err
		}
	}
	if pos < len(positionals) {
		// 位置参数缺省：必需参数缺失报错；可选（"?" 后缀）缺失放行。
		var missing []string
		for i := pos; i < len(positionals); i++ {
			if !strings.HasSuffix(positionals[i], "?") {
				missing = append(missing, positionals[i])
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing positional argument: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

// assign 把字符串值写入字段。
func assign(v reflect.Value, fl field, raw string) error {
	fv := v.FieldByIndex(fl.index)
	switch {
	case fl.isAny:
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			fv.Set(reflect.ValueOf(parsed))
		} else {
			fv.Set(reflect.ValueOf(raw))
		}
		return nil
	case fl.isList:
		parts := []string{}
		if raw != "" {
			parts = strings.Split(raw, ",")
		}
		list := reflect.MakeSlice(reflect.SliceOf(fl.elem), 0, len(parts))
		for _, p := range parts {
			ev := reflect.New(fl.elem).Elem()
			if err := assignScalar(ev, fl.elem.Kind(), p); err != nil {
				return err
			}
			list = reflect.Append(list, ev)
		}
		fv.Set(list)
		return nil
	default:
		return assignScalar(fv, fl.kind, raw)
	}
}

func assignScalar(fv reflect.Value, kind reflect.Kind, raw string) error {
	switch kind {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid boolean %q", raw)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		fv.SetInt(n)
	case reflect.Float64:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("invalid number %q", raw)
		}
		fv.SetFloat(n)
	default:
		return fmt.Errorf("unsupported field kind %s", kind)
	}
	return nil
}
