package proto

import "testing"

func TestSubjects(t *testing.T) {
	caps, err := CapsSubject("u1", "host_abc", 2)
	if err != nil || caps != "u.u1.h.host_abc.2.caps" {
		t.Errorf("CapsSubject = %q, %v", caps, err)
	}
	pres, err := PresenceSubject("u1", "host_abc", 1)
	if err != nil || pres != "u.u1.h.host_abc.1.presence" {
		t.Errorf("PresenceSubject = %q, %v", pres, err)
	}
	fs, err := FSReqSubject("u1", "s9", "host_abc")
	if err != nil || fs != "u.u1.s.s9.h.host_abc.fs.req" {
		t.Errorf("FSReqSubject = %q, %v", fs, err)
	}
	// 裸 host_id（1host 参数形态）自动加 HostIDPrefix；已带前缀输入幂等
	fs2, err := FSReqSubject("u1", "s9", "abc")
	if err != nil || fs2 != "u.u1.s.s9.h.host_abc.fs.req" {
		t.Errorf("FSReqSubject(raw id) = %q, %v", fs2, err)
	}
	ex, err := ExecReqSubject("u1", "s9", HostPage)
	if err != nil || ex != "u.u1.s.s9.h.page.exec.req" {
		t.Errorf("ExecReqSubject(page) = %q, %v", ex, err)
	}
	inbox, err := HostInboxSubject("u1", "host_abc")
	if err != nil || inbox != "u.u1.s.*.h.host_abc.>" {
		t.Errorf("HostInboxSubject = %q, %v", inbox, err)
	}
	inbox2, err := HostInboxSubject("u1", "abc")
	if err != nil || inbox2 != "u.u1.s.*.h.host_abc.>" {
		t.Errorf("HostInboxSubject(raw id) = %q, %v", inbox2, err)
	}
	allow, err := UserAllowPattern("u1")
	if err != nil || allow != "u.u1.>" {
		t.Errorf("UserAllowPattern = %q, %v", allow, err)
	}
	deny, err := FrontendDenyPattern("u1")
	if err != nil || deny != "u.u1.s.*.h.host_*.>" {
		t.Errorf("FrontendDenyPattern = %q, %v", deny, err)
	}
	if g := PageQueueGroup("s9"); g != "page-s9" {
		t.Errorf("PageQueueGroup = %q", g)
	}

	// 非法段拒绝：点号/通配符/空白/空段/0 credVer
	bad := []string{"a.b", "a*", "a>", "a b", ""}
	for _, b := range bad {
		if _, err := CapsSubject(b, "host_abc", 1); err == nil {
			t.Errorf("CapsSubject(%q) want error", b)
		}
		if _, err := ToolReqSubject("u1", b, "host_abc", ToolFS); err == nil {
			t.Errorf("ToolReqSubject sid=%q want error", b)
		}
	}
	if _, err := CapsSubject("u1", "host_abc", 0); err == nil {
		t.Error("credVer=0 want error")
	}
	if _, err := ToolReqSubject("u1", "s9", "host_abc", "git"); err == nil {
		t.Error("tool=git want error")
	}
}

func TestParseToolReqSubject(t *testing.T) {
	// host 段 host_{host_id} 解析后还原为裸 host_id（与 1host 参数一致）
	uid, sid, host, tool, err := ParseToolReqSubject("u.u1.s.s9.h.host_abc.exec.req")
	if err != nil || uid != "u1" || sid != "s9" || host != "abc" || tool != ToolExec {
		t.Errorf("parse = %q %q %q %q, %v", uid, sid, host, tool, err)
	}
	// page 保留字原样返回
	_, _, phost, _, err := ParseToolReqSubject("u.u1.s.s9.h.page.fs.req")
	if err != nil || phost != HostPage {
		t.Errorf("parse page = %q, %v", phost, err)
	}
	for _, s := range []string{
		"u.u1.h.host_abc.1.caps",       // 连接级
		"u.u1.s.s9.h.host_abc.git.req", // 非法指令集段
		"u.u1.s.s9.h.host_abc.fs",      // 缺 .req
		"u.u1.s.s9.h.host_abc.fs.req.extra",
	} {
		if _, _, _, _, err := ParseToolReqSubject(s); err == nil {
			t.Errorf("ParseToolReqSubject(%q) want error", s)
		}
	}
}
