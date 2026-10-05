package protocol

import "testing"

func TestSubjects(t *testing.T) {
	for _, tc := range []struct {
		want string
		call func() (string, error)
	}{
		{"u.u1.h.abc.2.caps", func() (string, error) { return CapsSubject("u1", "abc", 2) }},
		{"u.u1.h.abc.1.presence", func() (string, error) { return PresenceSubject("u1", "abc", 1) }},
		{"u.u1.h.host_abc.>", func() (string, error) { return HostInboxSubject("u1", "abc") }},
		{"u.u1.h.host_abc.rtc.in", func() (string, error) { return RtcInSubject("u1", "abc") }},
		{"u.u1.>", func() (string, error) { return UserAllowPattern("u1") }},
		{"u.u1.h.host_*.>", func() (string, error) { return FrontendDenyPattern("u1") }},
	} {
		got, err := tc.call()
		if err != nil || got != tc.want {
			t.Fatalf("%s %v", got, err)
		}
	}
	for _, bad := range []string{"a.b", "a*", "a>", "a b", ""} {
		if _, err := HostInboxSubject("u1", bad); err == nil {
			t.Fatal("invalid segment admitted")
		}
	}
	if _, err := CapsSubject("u1", "abc", 0); err == nil {
		t.Fatal("invalid credential version admitted")
	}
}
