// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package skillrun owns ZIP installation, command binding and package processes.
// Each package has one process/service provider; service ownership is independent
// of shell sessions and bg. Shared package schema lives in aic-skills/sdk/go/skillpkg.
package skillrun

import (
	"os"

	"github.com/veypi/aic-skills/sdk/go/skillpkg"
)

type (
	Manifest      = skillpkg.Manifest
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
