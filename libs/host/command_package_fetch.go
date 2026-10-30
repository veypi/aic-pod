package host

// command_package_fetch.go — 技能包获取（设备侧）：走 /fs/cloud 文件面。
//
// 为什么不用 /api/skills/{id}/download：设备凭证是 vbase 静态 key（kind=aic:device），
// 平台 /api 全树由 RequireLogin 挡住（只认 inherit 会话凭证），静态 key 天然进不去
// （403 no permission）。设备真正持有的授权面只有两个：
//   - /fs/cloud（fs:{user_id}:6）——文件读写面
//   - /api/nc（nc:{host_id}:2）——工具调用面
//
// 包目录在 UFS 物理布局里就是 /skills/{id}（aic/fs paths.go），私有包按 owner
// 读写判定、公开包只读、非 owner 私有恒拒（aic/fs skill 门）——ACL 与技能面同口径，
// 不因改传输面而放宽。
//
// 取包形态 = 一次目录列表（depth=5）+ 逐文件 GET，与 pull 同源；不引入 zip 中转，
// 免去平台的二次打包面。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// fetchCommandPackage 把 /skills/{skillID} 的整棵树取到 dest（目录必须已存在）。
// 尺寸闸门与取包路径安全：单文件 32 MiB、文件数 4096、总量 128 MiB。
func fetchCommandPackage(ctx context.Context, cloud *cloudFS, skillID, dest string) error {
	root := "/skills/" + skillID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloud.base+"/fs/cloud"+root+"?depth=5", nil)
	if err != nil {
		return err
	}
	resp, err := cloud.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-UFS-Dir") != "true" {
		return fmt.Errorf("skill package %s is not a directory", skillID)
	}
	var tree dirEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&tree); err != nil {
		return fmt.Errorf("parse skill package listing: %w", err)
	}
	var files int
	var total int64
	var walk func(entry dirEntry, prefix string) error
	walk = func(entry dirEntry, prefix string) error {
		for _, item := range entry.Items {
			name := strings.TrimSpace(item.Name)
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
				return fmt.Errorf("skill package contains invalid path: %q", item.Name)
			}
			rel := path.Join(prefix, name)
			if item.Dir {
				if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(rel)), 0o755); err != nil {
					return err
				}
				if err := walk(item, rel); err != nil {
					return err
				}
				continue
			}
			files++
			if files > maxCommandPackageFiles {
				return fmt.Errorf("skill package contains too many files")
			}
			if item.Size < 0 || item.Size > maxCommandPackageFile {
				return fmt.Errorf("skill package file %q exceeds 32 MiB", rel)
			}
			total += item.Size
			if total > maxCommandPackageContent {
				return fmt.Errorf("skill package content exceeds 128 MiB")
			}
			if err := downloadCloudFile(ctx, cloud, root+"/"+rel, filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(tree, "")
}

// downloadCloudFile 取单个云端文件到本地路径（覆盖写）。
func downloadCloudFile(ctx context.Context, cloud *cloudFS, cloudPath, localPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloud.base+"/fs/cloud"+cloudPath, nil)
	if err != nil {
		return err
	}
	resp, err := cloud.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-UFS-Dir") == "true" {
		return fmt.Errorf("%s is a directory", cloudPath)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(localPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(resp.Body, maxCommandPackageFile+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxCommandPackageFile {
		return fmt.Errorf("skill package file %q exceeds 32 MiB", cloudPath)
	}
	return nil
}
