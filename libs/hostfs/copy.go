package hostfs

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/libs/hostcmd"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	"github.com/veypi/aic-pod/protocol/hosts"
)

// A directory copy is a bounded sequence of conditional commits, not a
// transaction. Failure reports the number of entries already committed.
func (f *FS) copy(ctx context.Context, call hostcmd.Call, p moveArgs) (any, error) {
	if len(p.Dst.Segments) == 0 {
		return nil, hosts.Fail("permission_denied", "Cannot replace a root")
	}
	_, sourcePath, err := f.check(ctx, call, p.Src, false)
	if err != nil {
		return nil, err
	}
	_, targetPath, err := f.check(ctx, call, p.Dst, true)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(sourcePath, targetPath)
	if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return nil, hosts.Fail("invalid_argument", "Cannot copy into the source subtree")
	}
	info, err := f.info(ctx, call, p.Src, false)
	if err != nil {
		return nil, err
	}
	if version(info) != p.IfVersion {
		return nil, hosts.Fail("version_conflict", "Copy source changed")
	}
	if info.IsDir() && !p.Condition.Absent {
		return nil, hosts.Fail("unsupported", "Directory copy requires an absent destination")
	}
	destination := func(src fsp.Path) fsp.Path {
		return fsp.Path{RootID: p.Dst.RootID, Segments: append(append([]string{}, p.Dst.Segments...), src.Segments[len(p.Src.Segments):]...)}
	}
	var plan []walkEntry
	err = f.walk(ctx, call, p.Src, 256, false, false, func(e walkEntry) error {
		if !e.info.IsDir() && !e.info.Mode().IsRegular() {
			return hosts.Fail("unsupported", "Copy does not follow or recreate links")
		}
		if e.info.Mode().IsRegular() && e.info.Size() > 512<<20 {
			return hosts.Fail("overloaded", "Copy source exceeds 512 MiB per file")
		}
		if _, _, err := f.check(ctx, call, destination(e.path), true); err != nil {
			return err
		}
		plan = append(plan, e)
		return nil
	})
	if errors.Is(err, stopWalk) {
		return nil, hosts.Fail("overloaded", "Copy exceeds entry budget")
	}
	if err != nil {
		return nil, err
	}
	completed := 0
	for _, e := range plan {
		current, err := f.info(ctx, call, e.path, false)
		if err != nil {
			return nil, partial(err, completed)
		}
		if version(current) != version(e.info) {
			return nil, partial(hosts.Fail("source_changed", "Copy source changed"), completed)
		}
		dst := destination(e.path)
		if e.info.IsDir() {
			_, err = f.mkdir(ctx, call, mkdirArgs{Path: dst})
		} else {
			condition := fsp.Condition{Absent: true}
			if len(plan) == 1 {
				condition = p.Condition
			}
			err = f.copyFile(ctx, call, e.path, dst, version(e.info), condition)
		}
		if err != nil {
			return nil, partial(err, completed)
		}
		completed++
	}
	current, err := f.info(ctx, call, p.Dst, false)
	if err != nil {
		return nil, partial(err, completed)
	}
	return map[string]any{"entry": entry(p.Dst, current), "completed": completed}, nil
}

func (f *FS) copyFile(ctx context.Context, call hostcmd.Call, src, dst fsp.Path, version string, condition fsp.Condition) error {
	readCall := call
	readCall.Method = "read"
	value, err := f.readOrStat(ctx, readCall, pathArgs{Path: src, IfVersion: version})
	if err != nil {
		return err
	}
	live := value.(hostcmd.ByteSource)
	defer f.cfg.Bytes.Release(call.SessionID, live.Ref)
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := f.cfg.Bytes.Copy(ctx, call.SessionID, live.Ref, 0, nil, w)
		w.CloseWithError(err)
		done <- err
	}()
	sealed, err := f.cfg.Bytes.Upload(ctx, call.SessionID, r, &live.Size, "", live.MediaType)
	r.CloseWithError(err)
	readErr := <-done
	if err != nil {
		return err
	}
	defer f.cfg.Bytes.Release(call.SessionID, sealed.Ref)
	if readErr != nil {
		return readErr
	}
	_, err = f.write(ctx, call, writeArgs{Path: dst, Source: sealed.Ref, Condition: condition})
	return err
}
