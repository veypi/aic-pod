// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// browser-baseline 是 Browser v5（进程内服务形态）的性能基线探针
// （v6 P0⑤，aic/docs/todo.md）：直驱 browser.Service（v6 P5 起在
// skill-packages/browser/provider/browser）真实 Chrome，产出
// 命令延迟 / 输入→画面 p95 / 帧率 / CPU / 内存五项指标 JSON，供 P5 拆包
// （browser 移出为 skill 包）前后对比。P5 验收时用同一探针复测。
//
// 用法：
//
//	go run ./scripts/browser-baseline [-n 30] [-out docs/baselines/browser-v5-$(date +%F).json]
//
// 需要真实机环境（Chrome 可启动；沙箱化 agent 环境跑不了，请真机直跑）。
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/aic-pod/skill-packages/browser/provider/browser"
)

const fixtureHTML = `<!doctype html><title>Baseline</title>
<style>#spin{width:80px;height:80px;background:#36c;animation:r 1s linear infinite}@keyframes r{to{transform:rotate(360deg)}}</style>
<div id="spin"></div><div id="count">0</div>
<script>
let n = 0;
const bump = () => { document.getElementById('count').textContent = ++n; };
document.addEventListener('pointerdown', bump);
document.addEventListener('keydown', bump);
</script>`

type report struct {
	Date        string             `json:"date"`
	Form        string             `json:"form"` // v5 = 进程内服务
	Samples     int                `json:"samples"`
	LatencyMs   map[string]summary `json:"latency_ms"`
	InputFrame  summary            `json:"input_to_frame_ms"`
	FPS         float64            `json:"fps"`
	ChromeCPU   float64            `json:"chrome_cpu_pct"`
	ChromeRSSMB float64            `json:"chrome_rss_mb"`
}

type summary struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

func main() {
	n := flag.Int("n", 30, "每指标采样次数")
	out := flag.String("out", "", "结果 JSON 输出路径（空 = 仅 stdout）")
	flag.Parse()

	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(fixtureHTML))
	}))
	defer fixture.Close()

	stateDir, err := os.MkdirTemp("", "aic-baseline-")
	must(err)
	defer os.RemoveAll(stateDir)

	svc := browser.New(browser.Config{
		StateDir: filepath.Join(stateDir, "browser"),
		Width:    1280, Height: 720,
	})
	defer svc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	rep := &report{Date: time.Now().Format("2006-01-02"), Form: "v5-inproc", Samples: *n, LatencyMs: map[string]summary{}}

	// ---- create + wait_load（N=8，每轮新页面） ----
	var createSamples, loadSamples []float64
	var page browser.PageInfo
	for i := 0; i < 8; i++ {
		t0 := time.Now()
		p, err := svc.Create(ctx, browser.CreateArgs{URL: fixture.URL})
		must(err)
		createSamples = append(createSamples, ms(t0))
		t0 = time.Now()
		info, err := svc.Wait(ctx, browser.WaitArgs{PageID: p.ID, Load: true})
		must(err)
		loadSamples = append(loadSamples, ms(t0))
		if i == 0 {
			page = info // Create 返回的 Document 是导航前旧文档，Wait 后才是当前文档
			continue
		}
		_, err = svc.ClosePage(ctx, browser.PageArgs{PageID: p.ID})
		must(err)
	}
	rep.LatencyMs["create"] = summarize(createSamples)
	rep.LatencyMs["wait_load"] = summarize(loadSamples)

	// ---- observe / list / wait（N=n，已加载页面） ----
	rep.LatencyMs["observe"] = summarize(sample(*n, func() {
		_, err := svc.Observe(ctx, browser.ObserveArgs{PageID: page.ID})
		must(err)
	}))
	rep.LatencyMs["list"] = summarize(sample(*n, func() {
		_, err := svc.List(ctx, browser.Empty{})
		must(err)
	}))
	rep.LatencyMs["wait"] = summarize(sample(*n, func() {
		_, err := svc.Wait(ctx, browser.WaitArgs{PageID: page.ID, Load: true})
		must(err)
	}))

	// ---- 输入→画面（N=n）：Input stream 注入 pointer.down → 下一帧到达 ----
	frames, err := svc.Frames(ctx, browser.PageArgs{PageID: page.ID})
	must(err)
	defer frames.Close()
	input, err := svc.Input(ctx, "baseline", browser.PageArgs{PageID: page.ID})
	must(err)
	defer input.Close()
	// 排空初始帧，记录基线 seq
	var baseSeq uint64
	drainCtx, drainCancel := context.WithTimeout(ctx, 10*time.Second)
	for baseSeq == 0 {
		chunk, err := frames.Recv(drainCtx)
		must(err)
		baseSeq = chunkSeq(chunk)
	}
	drainCancel()

	var i2f []float64
	var inputSeq uint64 // 输入批序号（与帧序号是独立计数器，只需单调递增）
	for i := 0; i < *n; i++ {
		inputSeq++
		batch, _ := json.Marshal(map[string]any{
			"seq":         inputSeq,
			"document_id": page.Document,
			"events":      []map[string]any{{"type": "pointer.down", "x": 100, "y": 100, "button": "left"}},
		})
		t0 := time.Now()
		must(input.Send(ctx, batch))
		// 等 seq 更大的帧
		recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
		for {
			chunk, err := frames.Recv(recvCtx)
			must(err)
			if seq := chunkSeq(chunk); seq > baseSeq {
				i2f = append(i2f, ms(t0))
				baseSeq = seq
				break
			}
		}
		recvCancel()
		// 松开按键（防 held 状态泄漏到下一批）
		inputSeq++
		up, _ := json.Marshal(map[string]any{
			"seq":         inputSeq,
			"document_id": page.Document,
			"events":      []map[string]any{{"type": "pointer.up", "x": 100, "y": 100, "button": "left"}},
		})
		_ = input.Send(ctx, up)
	}
	rep.InputFrame = summarize(i2f)

	// ---- 帧率（3s 窗口，CSS 动画页）+ Chrome CPU/RSS 采样 ----
	frameCount := 0
	fpsCtx, fpsCancel := context.WithTimeout(ctx, 3*time.Second)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		cpu, rss := chromeStats(stateDir)
		rep.ChromeCPU, rep.ChromeRSSMB = cpu, rss
	}()
	for {
		if _, err := frames.Recv(fpsCtx); err != nil {
			break
		}
		frameCount++
	}
	fpsCancel()
	rep.FPS = float64(frameCount) / 3.0

	enc, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(enc))
	if *out != "" {
		must(os.MkdirAll(filepath.Dir(*out), 0o755))
		must(os.WriteFile(*out, enc, 0o644))
		fmt.Fprintln(os.Stderr, "written:", *out)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "baseline:", err)
		os.Exit(1)
	}
}

func ms(t0 time.Time) float64 { return float64(time.Since(t0).Microseconds()) / 1000.0 }

func sample(n int, f func()) []float64 {
	out := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		f()
		out = append(out, ms(t0))
	}
	return out
}

func summarize(xs []float64) summary {
	if len(xs) == 0 {
		return summary{}
	}
	sorted := append([]float64{}, xs...)
	sort.Float64s(sorted)
	pick := func(q float64) float64 {
		idx := int(q * float64(len(sorted)-1))
		return sorted[idx]
	}
	return summary{P50: pick(0.50), P95: pick(0.95), Min: sorted[0], Max: sorted[len(sorted)-1]}
}

// chunkSeq 解析帧块元数据的 frame_seq（帧块 = [4B 头长][JSON 头][JPEG]）。
func chunkSeq(chunk []byte) uint64 {
	if len(chunk) < 4 {
		return 0
	}
	hl := binary.BigEndian.Uint32(chunk[:4])
	if len(chunk) < 4+int(hl) {
		return 0
	}
	var header struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(chunk[4:4+hl], &header) != nil {
		return 0
	}
	var meta struct {
		Seq uint64 `json:"frame_seq"`
	}
	if json.Unmarshal(header.Metadata, &meta) != nil {
		return 0
	}
	return meta.Seq
}

// chromeStats 采样 Chrome 进程组 CPU%/RSS（pgrep user-data-dir 特征路径）。
func chromeStats(stateDir string) (cpu, rssMB float64) {
	out, err := exec.Command("pgrep", "-f", "user-data-dir="+filepath.Join(stateDir, "browser")).Output()
	if err != nil {
		return 0, 0
	}
	pids := strings.Join(strings.Fields(string(out)), ",")
	if pids == "" {
		return 0, 0
	}
	ps, err := exec.Command("ps", "-o", "pcpu=,rss=", "-p", pids).Output()
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(ps)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		c, _ := strconv.ParseFloat(f[0], 64)
		r, _ := strconv.ParseFloat(f[1], 64)
		cpu += c
		rssMB += r / 1024.0
	}
	return cpu, rssMB
}
