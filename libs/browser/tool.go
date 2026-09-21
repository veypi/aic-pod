package browser

import (
	"context"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
)

func (s *Service) Tool() tool.Command {
	spec := func(name, cli string, access int, pos ...string) tool.Spec {
		return tool.Spec{Background: name == "page.wait" || name == "download.wait" || name == "window.wait", Name: name, CLI: cli, Access: access, Positionals: pos}
	}
	t := tool.DefineCommand("browser",
		tool.Bind(spec("status", "status", 1), s.Status),
		tool.Bind(spec("page.list", "pages", 1), s.List),
		tool.Bind(spec("page.create", "open", 2, "url"), s.Create),
		tool.Bind(spec("page.navigate", "navigate", 2, "url"), s.Navigate),
		tool.Bind(spec("page.close", "close", 2), s.ClosePage),
		tool.Bind(spec("page.observe", "observe", 1), s.Observe),
		tool.Bind(spec("page.wait", "wait", 1), s.Wait),
		tool.Bind(spec("page.events", "events", 1), s.Events),
		tool.Bind(spec("page.dialog.resolve", "dialog", 2), s.Dialog),
		tool.Bind(spec("page.evaluate", "eval", 3, "code"), s.Evaluate),
		tool.Bind(spec("page.upload", "upload", 2), s.Upload),
		tool.Bind(spec("download.list", "downloads", 1), s.DownloadList),
		tool.Bind(spec("download.get", "download.get", 1, "download_id"), s.DownloadGet),
		tool.Bind(spec("download.wait", "download.wait", 1, "download_id"), s.DownloadWait),
		tool.Bind(spec("download.cancel", "download.cancel", 2, "download_id"), s.DownloadCancel),
		tool.Bind(spec("download.export", "download.export", 2, "download_id", "path"), s.DownloadExport),
		tool.BindStream(tool.Spec{Name: "page.frames", Access: 1}, s.Frames),
		tool.BindStream(tool.Spec{Name: "page.input", Access: 3}, s.Input),
		tool.Bind(tool.Spec{Name: "download.read", Access: 1}, s.DownloadRead),
	)
	for _, name := range []string{"click", "fill", "type", "press", "hover", "scroll", "drag", "set"} {
		t.Methods = append(t.Methods, tool.Bind(spec("page."+name, name, 2), s.action(name)))
	}
	for name, delta := range map[string]int{"back": -1, "forward": 1, "reload": 0} {
		t.Methods = append(t.Methods, tool.Bind(spec("page."+name, name, 2), func(ctx context.Context, c tool.Caller, a PageArgs) (Result, error) {
			return s.history(ctx, c, a, delta)
		}))
	}
	t.Close = s.Close
	return t
}
