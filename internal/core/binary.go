package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// DefaultVersion 是普通源码构建默认下载的 sing-box 版本。
const DefaultVersion = "1.14.0"

const minimumSupportedVersion = DefaultVersion

const downloadBaseURL = "https://github.com/SagerNet/sing-box/releases/download"
const releaseAPIBaseURL = "https://api.github.com/repos/SagerNet/sing-box/releases/tags"

// BinaryManager 负责 sing-box 二进制的发现、下载与版本管理。
type BinaryManager struct {
	paths      *platform.Paths
	proxy      string // 下载代理，如 http://127.0.0.1:7890；空为直连
	embeddedMu sync.Mutex

	versionMu    sync.Mutex
	versionCache versionCacheEntry
}

// versionCacheEntry 按二进制身份（路径 + mtime + 大小）缓存版本号。
// TUI 每帧渲染都会经 Manager.Status() 查询版本，没有缓存时每次都要
// 执行 `sing-box version` 子进程，滚动列表时明显卡顿。二进制被替换
// （下载更新 / 释放内置）后 mtime 或大小变化，缓存自动失效。
type versionCacheEntry struct {
	path    string
	modTime time.Time
	size    int64
	version string
}

// NewBinaryManager 创建二进制管理器。
func NewBinaryManager(paths *platform.Paths, downloadProxy string) *BinaryManager {
	return &BinaryManager{paths: paths, proxy: downloadProxy}
}

// SetProxy 更新用户配置的下载代理地址。
func (b *BinaryManager) SetProxy(downloadProxy string) {
	b.proxy = strings.TrimSpace(downloadProxy)
}

// ManagedPath 返回受管二进制的路径。
func (b *BinaryManager) ManagedPath() string { return b.paths.CoreBin }

// Installed 报告受管二进制是否存在且可执行。
func (b *BinaryManager) Installed() bool {
	info, err := os.Stat(b.paths.CoreBin)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// Resolve 返回可用的 sing-box 二进制路径：优先用户已安装版本，其次当前
// 构建内置的版本，最后回退到 PATH 中的 sing-box。
func (b *BinaryManager) Resolve() (string, error) {
	if b.Installed() {
		return b.paths.CoreBin, nil
	}
	if data, ok := embeddedBinary(); ok {
		if err := b.installEmbedded(data); err == nil {
			return b.paths.CoreBin, nil
		} else if p, pathErr := exec.LookPath("sing-box"); pathErr == nil {
			return p, nil
		} else {
			return "", fmt.Errorf("释放内置 sing-box 失败: %w", err)
		}
	}
	if p, err := exec.LookPath("sing-box"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 sing-box 二进制（内置版本不可用，%s 不存在，PATH 中也没有 sing-box），请先下载", b.paths.CoreBin)
}

func (b *BinaryManager) installEmbedded(data []byte) error {
	b.embeddedMu.Lock()
	defer b.embeddedMu.Unlock()
	if b.Installed() {
		return nil
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("打开内置 sing-box 压缩包失败: %w", err)
	}
	defer gz.Close()

	tmp, err := os.CreateTemp(filepath.Dir(b.paths.CoreBin), ".sing-box-embedded-*")
	if err != nil {
		return fmt.Errorf("创建内置 sing-box 临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("设置内置 sing-box 权限失败: %w", err)
	}
	if _, err := io.Copy(tmp, gz); err != nil {
		tmp.Close()
		return fmt.Errorf("释放内置 sing-box 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写入内置 sing-box 失败: %w", err)
	}
	if err := os.Rename(tmpPath, b.paths.CoreBin); err != nil {
		return fmt.Errorf("安装内置 sing-box 失败: %w", err)
	}
	return nil
}

// Version 返回 sing-box 版本号（如 "1.14.0"）。同一二进制的结果会被缓存，
// 避免每帧渲染都执行 `sing-box version` 子进程。
func (b *BinaryManager) Version(ctx context.Context) (string, error) {
	bin, err := b.Resolve()
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(bin); statErr == nil {
		b.versionMu.Lock()
		c := b.versionCache
		b.versionMu.Unlock()
		if c.path == bin && c.modTime.Equal(info.ModTime()) && c.size == info.Size() {
			return c.version, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	version, err := b.versionOf(ctx, bin)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(bin); statErr == nil {
		b.versionMu.Lock()
		b.versionCache = versionCacheEntry{path: bin, modTime: info.ModTime(), size: info.Size(), version: version}
		b.versionMu.Unlock()
	}
	return version, nil
}

func (b *BinaryManager) versionOf(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", fmt.Errorf("执行 sing-box version 失败: %w", err)
	}
	return ParseVersion(string(out))
}

// ValidateVersion rejects binaries older than the configuration target.
// Newer versions are accepted because the emitted fields remain compatible.
func (b *BinaryManager) ValidateVersion(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	got, err := b.versionOf(ctx, bin)
	if err != nil {
		return err
	}
	if compareVersions(got, minimumSupportedVersion) < 0 {
		return fmt.Errorf("sing-box 版本过低: %s，需要至少 %s", got, minimumSupportedVersion)
	}
	return nil
}

func compareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		na, nb := 0, 0
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
	}
	return 0
}

// ParseVersion 从 `sing-box version` 输出中解析版本号。
func ParseVersion(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		// 形如: sing-box version 1.14.0
		if len(fields) >= 3 && fields[0] == "sing-box" && fields[1] == "version" {
			return strings.TrimPrefix(fields[2], "v"), nil
		}
	}
	return "", fmt.Errorf("无法从输出中解析版本: %q", firstLine(output))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Ensure 确保存在可用的 sing-box 二进制，缺失时下载默认版本。
func (b *BinaryManager) Ensure(ctx context.Context) (string, error) {
	if path, err := b.Resolve(); err == nil {
		return path, nil
	}
	if err := b.Download(ctx, DefaultVersion); err != nil {
		return "", err
	}
	return b.paths.CoreBin, nil
}

// Download 下载并解压指定版本的 sing-box 到受管路径。
func (b *BinaryManager) Download(ctx context.Context, version string) error {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("当前平台 %q 不受支持（仅支持 Linux 和 macOS）", runtime.GOOS)
	}
	version = strings.TrimPrefix(version, "v")
	archiveExt := "tar.gz"
	name := fmt.Sprintf("sing-box-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	archiveName := name + "." + archiveExt
	downloadURL := fmt.Sprintf("%s/v%s/%s", downloadBaseURL, version, archiveName)

	// 始终显式设置 Transport，避免 net/http 的 DefaultTransport 读取环境代理。
	transport, err := newHTTPTransport(b.proxy)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Minute}
	expectedSHA256, err := fetchReleaseAssetSHA256(ctx, client, releaseAPIURL(version), archiveName)
	if err != nil {
		return fmt.Errorf("获取 sing-box 发布物校验和失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("构造下载请求失败: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载 %s 失败: %w", downloadURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 失败: HTTP %d", downloadURL, resp.StatusCode)
	}

	// 先落盘临时文件再解压，避免半截下载。
	tmpArchive, err := os.CreateTemp(filepath.Dir(b.paths.CoreBin), "sing-box-dl-*."+archiveExt)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpArchivePath := tmpArchive.Name()
	defer os.Remove(tmpArchivePath)

	if _, err := io.Copy(tmpArchive, resp.Body); err != nil {
		tmpArchive.Close()
		return fmt.Errorf("保存下载内容失败: %w", err)
	}
	if err := tmpArchive.Close(); err != nil {
		return fmt.Errorf("写入下载内容失败: %w", err)
	}
	if err := verifySHA256File(tmpArchivePath, expectedSHA256); err != nil {
		return fmt.Errorf("sing-box 发布物校验失败: %w", err)
	}

	if err := extractBinary(tmpArchivePath, b.paths.CoreBin); err != nil {
		return err
	}
	if err := os.Chmod(b.paths.CoreBin, 0o755); err != nil {
		return fmt.Errorf("设置二进制可执行权限失败: %w", err)
	}
	return nil
}

func releaseAPIURL(version string) string {
	return fmt.Sprintf("%s/v%s", releaseAPIBaseURL, strings.TrimPrefix(version, "v"))
}

type githubRelease struct {
	Assets []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// fetchReleaseAssetSHA256 获取 GitHub 为 release asset 记录的 SHA-256 digest。
// 这比依赖一个额外的 checksum 文件更可靠：sing-box 的发布物并不固定包含
// SHA256SUMS/checksums.txt，而 GitHub API 的 digest 与具体 asset 一一对应。
func fetchReleaseAssetSHA256(ctx context.Context, client *http.Client, apiURL, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("构造发布信息请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", DefaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求发布信息失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("请求发布信息失败: HTTP %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&release); err != nil {
		return "", fmt.Errorf("解析发布信息失败: %w", err)
	}
	for _, asset := range release.Assets {
		if asset.Name != assetName {
			continue
		}
		if asset.Digest == "" {
			return "", fmt.Errorf("发布物 %q 未提供 SHA-256 digest", assetName)
		}
		return normalizeSHA256(asset.Digest)
	}
	return "", fmt.Errorf("发布信息中未找到发布物 %q", assetName)
}

func normalizeSHA256(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "sha256:"), "SHA256:")
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("SHA-256 digest 长度非法: %q", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("SHA-256 digest 格式非法: %w", err)
	}
	return strings.ToLower(value), nil
}

func verifySHA256File(path, expected string) error {
	expected, err := normalizeSHA256(expected)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("计算 SHA-256 失败: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SHA-256 不匹配: 期望 %s，实际 %s", expected, actual)
	}
	return nil
}

// extractBinary 从压缩包中提取 sing-box 二进制到 dest。
func extractBinary(archivePath, dest string) error {
	return extractFromTarGz(archivePath, dest)
}

func extractFromTarGz(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开压缩包失败: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压 gzip 失败: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 条目失败: %w", err)
		}
		if filepath.Base(hdr.Name) == "sing-box" && !hdr.FileInfo().IsDir() {
			return writeStreamToFile(tr, dest)
		}
	}
	return fmt.Errorf("压缩包中未找到 sing-box 二进制")
}

func writeStreamToFile(r io.Reader, dest string) error {
	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("创建临时二进制文件失败: %w", err)
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("写入二进制失败: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("写入二进制失败: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换二进制失败: %w", err)
	}
	return nil
}
