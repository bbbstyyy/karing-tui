package core

import (
	"embed"
	"runtime"
)

// The release build populates a temporary copy of this directory with the
// target's compressed sing-box executable before compiling karing-tui.
// Ordinary source builds fall back to PATH or the download mechanism.
//
//go:embed embedded/*
var embeddedFiles embed.FS

// EmbeddedVersion is injected by the release build to match its kernel.
var EmbeddedVersion = DefaultVersion

func embeddedAssetName() string {
	return "embedded/sing-box-" + runtime.GOOS + "-" + runtime.GOARCH + ".bin.gz"
}

func embeddedBinary() ([]byte, bool) {
	data, err := embeddedFiles.ReadFile(embeddedAssetName())
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// EmbeddedAvailable reports whether this build contains a kernel for the
// current platform.
func EmbeddedAvailable() bool {
	_, ok := embeddedBinary()
	return ok
}
