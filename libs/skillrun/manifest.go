// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package skillrun 是 skill 包在 pod 端的安装/注册/运行权威（aic/docs/skill.md
// §9.2，v6）：包命令注册表生命周期收归本包一处——manifest 解析校验、包名冲突
// 检查（vs 内建/保留名/已装包）、根命令注册（vsh RegisterGuarded）、禁用=保留
// 注册显式失败、卸载=解注册（+bg kill，service 于 P0b 接线）、整包替换。
//
// 运行形态：
//   - process 类：每调用一次独立 vbox 沙箱进程（与 native 命令同一沙箱派生——
//     策略快照源复用 host.nativePolicy），stdin/stdout/stderr 直通引擎管道，
//     ctx 取消即杀进程组（受管取消）。
//   - service 类：首调用懒启动驻留 + bg 登记 + skillproc 拨号（P0b）。
//
// 根命令 = 包名（隐式，manifest 无 commands[]）；argv/stdin 全量透传给包的
// 默认 provider（providers[0]），子命令与 --help 由包 CLI 自行实现。
//
// cli/manifest.json 与 artifacts.lock.json 的 schema/校验真相源 =
// protocol/skillpkg（与 aic skillhub 共享，本文件只做别名与文件读取封装）。
package skillrun

import (
	"os"

	"github.com/veypi/aic-pod/protocol/skillpkg"
)

type (
	Manifest      = skillpkg.Manifest
	Provider      = skillpkg.Provider
	StreamDecl    = skillpkg.StreamDecl
	ArtifactsLock = skillpkg.ArtifactsLock
	Artifact      = skillpkg.Artifact
)

const (
	KindProcess = skillpkg.KindProcess
	KindService = skillpkg.KindService
)

// ParseManifest 读取并校验 cli/manifest.json 文件。
func ParseManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return skillpkg.ParseManifest(b)
}

// ParseManifestBytes 解析并校验 manifest JSON 字节。
func ParseManifestBytes(b []byte) (*Manifest, error) { return skillpkg.ParseManifest(b) }

// ParseArtifactsLock 解析并全量校验 artifacts.lock.json 字节。
func ParseArtifactsLock(data []byte) (*ArtifactsLock, error) {
	return skillpkg.ParseArtifactsLock(data)
}
