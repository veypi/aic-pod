package host

// cua 测试：argv→MCP 装配（纯函数）+ 探测 + 真机集成（本机装了 cua-driver 才跑，
// 经 runCua 走完整链路：懒启动 → 握手 → tools/call → 应答整形）。

import (
	"reflect"
	"testing"
)

func TestCuaMcpArgs(t *testing.T) {
	cases := []struct {
		name string
		goos string
		sock string
		want []string
	}{
		{"darwin daemon 唯一形态", "darwin", "/tmp/x.sock", []string{"mcp", "--socket", "/tmp/x.sock"}},
		{"linux 保留 direct 过渡", "linux", "", []string{"mcp", "--direct"}},
		{"windows 保留 direct 过渡", "windows", "", []string{"mcp", "--direct"}},
	}
	for _, c := range cases {
		if got := cuaMcpArgs(c.goos, c.sock); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// daemon 自动拉起参数（纯函数）：空 appPath = 用户自装形态（-a CuaDriver）；
// 非空 = 桌面端内置 app 路径直启（open -n -g <app> --args serve --grant ...）。
func TestCuaDaemonLaunchArgs(t *testing.T) {
	cases := []struct {
		name    string
		appPath string
		want    []string
	}{
		{"用户自装（-a CuaDriver）", "", []string{"-n", "-g", "-a", "CuaDriver", "--args", "serve"}},
		{"内置 app 路径直启", "/app/Resources/cua/darwin/CuaDriver.app", []string{"-n", "-g", "/app/Resources/cua/darwin/CuaDriver.app", "--args", "serve"}},
	}
	for _, c := range cases {
		if got := cuaDaemonLaunchArgs(c.appPath); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: cuaDaemonLaunchArgs(%q) = %v, want %v", c.name, c.appPath, got, c.want)
		}
	}
}
