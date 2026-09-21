package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type brandVersion struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

type userAgentMetadata struct {
	Brands          []brandVersion `json:"brands"`
	FullVersionList []brandVersion `json:"fullVersionList"`
	FullVersion     string         `json:"fullVersion"`
	Platform        string         `json:"platform"`
	PlatformVersion string         `json:"platformVersion"`
	Architecture    string         `json:"architecture"`
	Model           string         `json:"model"`
	Mobile          bool           `json:"mobile"`
	Bitness         string         `json:"bitness"`
	Wow64           bool           `json:"wow64"`
	FormFactors     []string       `json:"formFactors,omitempty"`
}

type identity struct {
	UserAgent string            `json:"userAgent"`
	Metadata  userAgentMetadata `json:"userAgentMetadata"`
	Window    windowMetrics     `json:"window"`
}

type windowMetrics struct {
	OuterWidth, OuterHeight   int
	InnerWidth, InnerHeight   int
	ScreenWidth, ScreenHeight int
}

func (v *identity) normalize() error {
	if v.UserAgent == "" || len(v.Metadata.Brands) == 0 || v.Metadata.FullVersion == "" {
		return fmt.Errorf("Chrome did not provide its native user agent metadata")
	}
	v.UserAgent = strings.ReplaceAll(v.UserAgent, "HeadlessChrome/", "Chrome/")
	for _, list := range [][]brandVersion{v.Metadata.Brands, v.Metadata.FullVersionList} {
		for i := range list {
			if list[i].Brand == "HeadlessChrome" {
				list[i].Brand = "Google Chrome"
			}
		}
	}
	return nil
}

// Read the running browser's own metadata, including GREASE brands, actual CPU
// architecture and OS version. about:blank has no secure context for UA-CH;
// this temporary built-in page needs neither network access nor a local server.
const readIdentity = `(async () => {
  if (!navigator.userAgentData) return null;
  const data = await navigator.userAgentData.getHighEntropyValues([
    'architecture', 'bitness', 'model', 'platformVersion', 'fullVersionList',
    'uaFullVersion', 'wow64', 'formFactors'
  ]);
  return {userAgent: navigator.userAgent,
    userAgentMetadata: {...data, fullVersion: data.uaFullVersion},
    window: {outerWidth, outerHeight, innerWidth, innerHeight,
      screenWidth: screen.width, screenHeight: screen.height}};
})()`

// Start never exposes a remote-debugging TCP port or uses a personal profile.
// Probe an isolated built-in page first, then launch the durable profile with
// its native UA normalized at process level. This also covers worker script
// requests which Chrome sends before a CDP target exists. No website JS patches.
func Start(ctx context.Context, path, profile string, args ...string) (*Conn, error) {
	dir, err := os.MkdirTemp("", "aic-chrome-identity-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	probe, err := start(ctx, path, dir, args...)
	if err != nil {
		return nil, err
	}
	v, err := probe.readIdentity(ctx)
	closeErr := probe.Close()
	if err != nil {
		return nil, fmt.Errorf("Chrome identity: %w", err)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	c, err := start(ctx, path, profile, append(args, "--user-agent="+v.UserAgent)...)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.identity = v
	c.mu.Unlock()
	if err = c.autoAttach(ctx, ""); err != nil {
		c.Close()
		return nil, fmt.Errorf("Chrome target initialization: %w", err)
	}
	return c, nil
}

func (c *Conn) readIdentity(ctx context.Context) (*identity, error) {
	var target struct {
		ID string `json:"targetId"`
	}
	if err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": "chrome://version"}, &target); err != nil {
		return nil, err
	}
	defer func() {
		if target.ID == "" {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Call(cleanup, "", "Target.closeTarget", map[string]any{"targetId": target.ID}, nil)
	}()
	var attached struct {
		Session string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &attached); err != nil {
		return nil, err
	}
	var v *identity
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for v == nil {
		var result struct {
			Result struct {
				Value *identity `json:"value"`
			} `json:"result"`
		}
		err := c.Call(ctx, attached.Session, "Runtime.evaluate", map[string]any{"expression": readIdentity, "returnByValue": true, "awaitPromise": true}, &result)
		if err == nil {
			v = result.Result.Value
		}
		if v == nil {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("read native metadata: %w", ctx.Err())
			case <-c.done:
				return nil, fmt.Errorf("Chrome closed while reading native metadata")
			case <-ticker.C:
			}
		}
	}
	if err := v.normalize(); err != nil {
		return nil, err
	}
	// Close before auto-attachment: the probe must never become a user page.
	if err := c.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": target.ID}, nil); err != nil {
		return nil, err
	}
	target.ID = ""
	return v, nil
}

func (c *Conn) autoAttach(ctx context.Context, session string) error {
	return c.Call(ctx, session, "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []map[string]any{{"type": "page"}, {"type": "iframe"}, {"type": "worker"}, {"type": "shared_worker"}, {"type": "service_worker"}, {"exclude": true}},
	}, nil)
}

// Native CDP overrides cover both JavaScript and HTTP headers. Setting only a
// --user-agent flag would discard Chrome's high-entropy Client Hints.
func (c *Conn) ApplyIdentity(ctx context.Context, session string) error {
	c.mu.Lock()
	v := c.identity
	c.mu.Unlock()
	if v == nil {
		return fmt.Errorf("Chrome identity not initialized")
	}
	params := map[string]any{
		"userAgent": v.UserAgent, "userAgentMetadata": v.Metadata,
	}
	// Browser-side requests (e.g. worker script loads) and renderer-side
	// navigator values have distinct overrides in Chromium.
	if err := c.Call(ctx, session, "Network.setUserAgentOverride", params, nil); err != nil {
		return err
	}
	return c.Call(ctx, session, "Emulation.setUserAgentOverride", params, nil)
}

// Keep each page's native window and emulated screen large enough for its
// configured viewport. Browser decorations come from Chrome, not a guessed OS.
func (c *Conn) ConfigurePage(ctx context.Context, target, session string, width, height int) error {
	if err := c.ApplyIdentity(ctx, session); err != nil {
		return err
	}
	c.mu.Lock()
	native := c.identity.Window
	c.mu.Unlock()
	outerWidth := width + max(0, native.OuterWidth-native.InnerWidth)
	outerHeight := height + max(0, native.OuterHeight-native.InnerHeight)
	var window struct {
		ID int `json:"windowId"`
	}
	if err := c.Call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": target}, &window); err != nil {
		return err
	}
	if err := c.Call(ctx, "", "Browser.setWindowBounds", map[string]any{
		"windowId": window.ID, "bounds": map[string]any{"left": 0, "top": 0, "width": outerWidth, "height": outerHeight},
	}, nil); err != nil {
		return err
	}
	return c.Call(ctx, session, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": width, "height": height, "deviceScaleFactor": 0, "mobile": false,
		"screenWidth": max(native.ScreenWidth, outerWidth), "screenHeight": max(native.ScreenHeight, outerHeight),
		"positionX": 0, "positionY": 0,
	}, nil)
}

// Pause child execution, configure metadata, then resume. The process-level UA
// covers earlier requests. Recurse for OOPIFs and nested workers, since root
// auto-attach is not recursive.
// Runs outside the CDP reader so replies can continue to be processed.
func (c *Conn) prepareAttachment(e Event) bool {
	if e.Method != "Target.attachedToTarget" {
		return false
	}
	c.mu.Lock()
	ready := c.identity != nil
	c.mu.Unlock()
	if !ready {
		return false
	}
	var event struct {
		Session string `json:"sessionId"`
		Waiting bool   `json:"waitingForDebugger"`
		Info    struct {
			Type string `json:"type"`
		} `json:"targetInfo"`
	}
	if json.Unmarshal(e.Params, &event) != nil || event.Session == "" {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stage := "user agent"
		var err error
		if event.Info.Type == "service_worker" {
			stage = "startup"
			err = c.prepareServiceWorker(ctx, event.Session)
		} else {
			err = c.ApplyIdentity(ctx, event.Session)
		}
		if err == nil && event.Waiting && event.Info.Type != "service_worker" {
			stage = "child attachment"
			err = c.autoAttach(ctx, event.Session)
		}
		if err == nil && event.Info.Type != "service_worker" {
			stage = "resume"
			err = c.Call(ctx, event.Session, "Runtime.runIfWaitingForDebugger", map[string]any{}, nil)
		}
		if err != nil && !strings.Contains(err.Error(), "Session with given id not found") && !strings.Contains(err.Error(), "Target closed") {
			c.fail(fmt.Errorf("configure Chrome %s %s: %w", event.Info.Type, stage, err))
		}
	}()
	return true
}

func (c *Conn) prepareServiceWorker(ctx context.Context, session string) error {
	c.mu.Lock()
	v := c.identity
	c.mu.Unlock()
	// Queue both overrides before resuming. Waiting for either reply first can
	// deadlock: the service worker renderer has not started yet.
	networkDone := c.queueCall(ctx, session, "Network.setUserAgentOverride", map[string]any{
		"userAgent": v.UserAgent, "userAgentMetadata": v.Metadata,
	}, nil)
	uaDone := c.queueCall(ctx, session, "Emulation.setUserAgentOverride", map[string]any{
		"userAgent": v.UserAgent, "userAgentMetadata": v.Metadata,
	}, nil)
	resumeErr := c.Call(ctx, session, "Runtime.runIfWaitingForDebugger", map[string]any{}, nil)
	networkErr, uaErr := networkDone(), uaDone()
	for _, err := range []error{resumeErr, networkErr, uaErr} {
		if err != nil {
			return err
		}
	}
	return nil
}
