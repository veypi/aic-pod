package execution

// quota_bypass_test.go 是 todo 4.2.5/4.1.11 的配额绕过实测（引擎集成层）：
// backing 换成「超限即拒写」的配额闸门测试件（语义对齐 QuotaFS：跨过配额的
// 那次写放行、之后的写请求失败），验证三条典型绕过路径——重定向追加、
// curl -o 大文件、tar 解包——的每一次落盘写都必经 backing 闸门，无处可绕。

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veypi/vbox"
	"github.com/veypi/vigo/contrib/ufs"
	gbfs "github.com/veypi/vsh/fs"
)

// errTestQuota 配额闸门测试件的拒写错误（对位 aicfs.ErrStorageQuotaExceeded）。
var errTestQuota = errors.New("storage quota exceeded")

// quotaTestFS 配额闸门测试件：usage > limit 后一切写入口（WriteFile 整写 /
// Create 流式逐块）拒绝；usage 只增不减（RemoveAll 语义不在本测试范围）。
type quotaTestFS struct {
	ufs.FS
	mu    sync.Mutex
	used  int64
	limit int64
}

func (q *quotaTestFS) check() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit >= 0 && q.used > q.limit {
		return errTestQuota
	}
	return nil
}

func (q *quotaTestFS) add(n int64) {
	q.mu.Lock()
	q.used += n
	q.mu.Unlock()
}

func (q *quotaTestFS) usage() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used
}

func (q *quotaTestFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if err := q.check(); err != nil {
		return err
	}
	if err := q.FS.WriteFile(name, data, perm); err != nil {
		return err
	}
	q.add(int64(len(data)))
	return nil
}

func (q *quotaTestFS) Create(name string) (ufs.File, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	f, err := q.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return &quotaTestFile{File: f, q: q}, nil
}

// quotaTestFile 流式写逐块过闸门（curl -o 主路径）。
type quotaTestFile struct {
	ufs.File
	q *quotaTestFS
}

func (f *quotaTestFile) Write(p []byte) (int, error) {
	if err := f.q.check(); err != nil {
		return 0, err
	}
	n, err := f.File.Write(p)
	f.q.add(int64(n))
	return n, err
}

// newQuotaTestEngine 构造接配额闸门 backing 的 cloud 形态引擎（规则表
// DefaultWrite 放行——本测试只验配额维度，权限维度有独立用例）。
func newQuotaTestEngine(t *testing.T, q *quotaTestFS) *Engine {
	t.Helper()
	e, err := NewEngine(EngineConfig{
		BaseEnv: map[string]string{"HOME": "/u/u1", "PATH": "/usr/bin:/bin"},
		NewSessionFS: func(ctx context.Context, key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewCloudFS(CloudFSConfig{
				UserRoot: "/u/u1",
				Backing:  q,
				Rules:    func() vbox.FSRuleSet { return vbox.FSRuleSet{DefaultWrite: vbox.EffRW} },
			})
			return fsys, "/u/u1", err
		},
		Network: NewNetClient(NetClientConfig{AllowPrivate: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// 重定向追加（>>）：跨多次 exec 逐次追加，配额用尽后追加必须失败且
// usage 停止增长——每一次写都过 backing 闸门，跨 exec 累计可见。
func TestQuotaBypassRedirectAppend(t *testing.T) {
	t.Parallel()
	base, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := &quotaTestFS{FS: base, limit: 128}
	e := newQuotaTestEngine(t, q)
	ctx := context.Background()

	failed := false
	for i := 0; i < 32; i++ {
		res, err := e.Exec(ctx, ExecRequest{
			SessionKey: "quota", Timeout: time.Minute,
			Script: fmt.Sprintf("echo line-%02d-padding-padding >> /u/u1/f.log", i),
		})
		if err != nil || res.ExitCode != 0 {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("配额用尽后重定向追加仍成功——闸门被绕过")
	}
	used := q.usage()
	if used > 128+64 {
		t.Fatalf("超限后仍继续落盘：used=%d", used)
	}
	// 闸门锁死：后续任何写（新文件/追加）都失败。
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "quota", Timeout: time.Minute, Script: "echo x > /u/u1/new.txt"})
	if err == nil && res.ExitCode == 0 {
		t.Fatal("超限后新文件写入仍成功")
	}
	if got := q.usage(); got != used {
		t.Fatalf("被拒写仍计入用量：%d→%d", used, got)
	}
}

// curl -o 大文件：写盘必经 backing 闸门——整写计入用量、超限后的后续写
// 锁死。（curl 实现是全量缓冲响应后单次整写：「跨过配额的那次写放行」是
// QuotaFS 文档化语义，单次跨线体积上限 = MaxResponseBytes。）
func TestQuotaBypassCurlDownload(t *testing.T) {
	t.Parallel()
	base, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := &quotaTestFS{FS: base, limit: 4096}
	e := newQuotaTestEngine(t, q)

	body := bytes.Repeat([]byte("A"), 512*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	res, err := e.Exec(context.Background(), ExecRequest{
		SessionKey: "quota", Timeout: time.Minute,
		Script: "curl -o /u/u1/big.bin " + srv.URL,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("首个跨线写按语义放行：err=%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr)
	}
	// 闸门可见：整写计入用量（未过闸门的写不会出现在计数里）。
	if got := q.usage(); got != int64(len(body)) {
		t.Fatalf("curl 落盘未过闸门计数：usage=%d want=%d", got, len(body))
	}
	// 超限锁死：后续任何写失败且报错含配额语义。
	res, err = e.Exec(context.Background(), ExecRequest{
		SessionKey: "quota", Timeout: time.Minute,
		Script: "curl -o /u/u1/big2.bin " + srv.URL,
	})
	if err == nil && res.ExitCode == 0 {
		t.Fatal("超限后 curl -o 仍成功——闸门被绕过")
	}
	if _, statErr := base.Stat("/u/u1/big2.bin"); statErr == nil {
		t.Fatal("被拒的 curl 写不应落盘")
	}
	t.Logf("curl 跨线写计入 usage=%d（配额 4096），后续写已锁死", q.usage())
}

// tar 解包：归档内每个成员的写都过闸门，配额用尽即中止，不得解全。
func TestQuotaBypassTarExtract(t *testing.T) {
	t.Parallel()
	base, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 种子归档直写底层 FS（不计用量——对位「存量文件不占当次写入判定」）。
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := 0; i < 8; i++ {
		content := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		name := fmt.Sprintf("f%d.bin", i)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := base.MkdirAll("/u/u1", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := base.WriteFile("/u/u1/a.tar", buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	q := &quotaTestFS{FS: base, limit: 8192} // 8×4096=32KiB 内容，8KiB 配额
	e := newQuotaTestEngine(t, q)
	res, err := e.Exec(context.Background(), ExecRequest{
		SessionKey: "quota", Timeout: time.Minute,
		Script: "cd /u/u1 && tar -xf a.tar",
	})
	if err == nil && res.ExitCode == 0 {
		t.Fatalf("tar 解包 32KiB 在 8KiB 配额下成功——闸门被绕过（usage=%d）", q.usage())
	}
	// 解包中止：成员不得全部落盘（逐个 Stat 数实际落盘数）。
	extracted := 0
	for i := 0; i < 8; i++ {
		if _, err := base.Stat(fmt.Sprintf("/u/u1/f%d.bin", i)); err == nil {
			extracted++
		}
	}
	if extracted >= 8 {
		t.Fatalf("配额内不应解出全部成员：%d/8", extracted)
	}
	t.Logf("tar 中止：解出 %d/8 成员，usage=%d（配额 8192）", extracted, q.usage())
}

// 配额错误可读：拒写报错须含配额语义，不是通用 IO 错（用户能看懂发生了什么）。
func TestQuotaErrorReadable(t *testing.T) {
	t.Parallel()
	base, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := &quotaTestFS{FS: base, limit: 0} // 任何写都超限（used=0 不 > 0？首写放行跨线）
	q.used = 1                            // 已超限：首写即拒
	e := newQuotaTestEngine(t, q)
	res, err := e.Exec(context.Background(), ExecRequest{
		SessionKey: "quota", Timeout: time.Minute,
		Script: "echo x > /u/u1/f.txt",
	})
	if err == nil && res.ExitCode == 0 {
		t.Fatal("超限写应失败")
	}
	stderr := ""
	if res != nil {
		stderr = res.Stderr
	}
	if err == nil && !strings.Contains(stderr, "quota") {
		t.Fatalf("报错应含配额语义：err=%v stderr=%s", err, stderr)
	}
}
