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
	cmp, err := compareVersions(got, minimumSupportedVersion)
	if err != nil {
		return fmt.Errorf("sing-box 版本校验失败: %w", err)
	}
	if cmp < 0 {
		return fmt.Errorf("sing-box 版本过低: %s，需要至少 %s", got, minimumSupportedVersion)
	}
	return nil
}

// semVersion 是按 SemVer 2.0.0 解析后的版本号。
type semVersion struct {
	major, minor, patch uint64
	prerelease          []string // 按 "." 切分的预发布标识符；nil 表示正式版
}

// parseSemVersion 按 SemVer 2.0.0 解析版本号，接受可选的前导 "v"。
// build metadata（"+" 之后）不参与比较，解析时丢弃。
// 与旧的宽松三段数值比较不同：预发布后缀参与比较（1.14.0-beta.1 < 1.14.0），
// 省略 patch 的输入（如 "1.15"）按非法处理——宽松输入应在 ParseVersion
// 输入层先规范化。
func parseSemVersion(s string) (semVersion, error) {
	s = strings.TrimPrefix(s, "v")
	core, pre, hasPre := s, "", false
	if i := strings.IndexByte(s, '+'); i >= 0 { // build metadata 不参与比较
		s = s[:i]
		core = s
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core, pre, hasPre = s[:i], s[i+1:], true
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semVersion{}, fmt.Errorf("版本号必须是 major.minor.patch 三段")
	}
	var nums [3]uint64
	for i, p := range parts {
		n, err := parseSemNum(p)
		if err != nil {
			return semVersion{}, fmt.Errorf("版本号第 %d 段 %q 非法: %w", i+1, p, err)
		}
		nums[i] = n
	}
	v := semVersion{major: nums[0], minor: nums[1], patch: nums[2]}
	if hasPre {
		if pre == "" {
			return semVersion{}, fmt.Errorf("预发布段为空")
		}
		v.prerelease = strings.Split(pre, ".")
		for _, id := range v.prerelease {
			if id == "" {
				return semVersion{}, fmt.Errorf("预发布标识符为空")
			}
			if isSemNum(id) {
				if len(id) > 1 && id[0] == '0' { // 严格 SemVer：数值标识符不得有前导零
					return semVersion{}, fmt.Errorf("预发布数值标识符 %q 有前导零", id)
				}
			} else if !isSemIdent(id) {
				return semVersion{}, fmt.Errorf("预发布标识符 %q 含非法字符", id)
			}
		}
	}
	return v, nil
}

func parseSemNum(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("空数字段")
	}
	if len(s) > 1 && s[0] == '0' { // 严格 SemVer：核心版本号不得有前导零
		return 0, fmt.Errorf("前导零")
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("不是十进制数字: %w", err)
	}
	return n, nil
}

func isSemNum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isSemIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// compareSemVersion 按 SemVer 2.0.0 precedence 比较两个已解析版本：
// 核心版本号按数值比较；正式版 > 预发布；预发布标识符中数值按数值比较、
// 数值段 < 非数值段、其余按字典序；标识符更少的一方更小。
func compareSemVersion(a, b semVersion) int {
	if a.major != b.major {
		return cmpUint(a.major, b.major)
	}
	if a.minor != b.minor {
		return cmpUint(a.minor, b.minor)
	}
	if a.patch != b.patch {
		return cmpUint(a.patch, b.patch)
	}
	switch {
	case len(a.prerelease) == 0 && len(b.prerelease) == 0:
		return 0
	case len(a.prerelease) == 0: // 正式版 > 预发布
		return 1
	case len(b.prerelease) == 0:
		return -1
	}
	for i := 0; i < len(a.prerelease) && i < len(b.prerelease); i++ {
		x, y := a.prerelease[i], b.prerelease[i]
		xn, yn := isSemNum(x), isSemNum(y)
		switch {
		case xn && yn:
			// 已排除前导零，规范十进制串：先比长度再比字典序即数值比较。
			if len(x) != len(y) {
				if len(x) < len(y) {
					return -1
				}
				return 1
			}
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		case xn != yn: // 数值段 < 非数值段
			if xn {
				return -1
			}
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	if len(a.prerelease) < len(b.prerelease) {
		return -1
	}
	if len(a.prerelease) > len(b.prerelease) {
		return 1
	}
	return 0
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareVersions 按 SemVer 2.0.0 precedence 比较两个版本号，接受可选的
// 前导 "v"；任一输入无法解析时返回错误，不再静默按 0 处理。
// build metadata（如 1.14.0+build.1）不参与比较。
func compareVersions(a, b string) (int, error) {
	va, err := parseSemVersion(a)
	if err != nil {
		return 0, fmt.Errorf("解析版本 %q 失败: %w", a, err)
	}
	vb, err := parseSemVersion(b)
	if err != nil {
		return 0, fmt.Errorf("解析版本 %q 失败: %w", b, err)
	}
	return compareSemVersion(va, vb), nil
}

// ParseVersion 从 `sing-box version` 输出中解析版本号。
// sing-box 可能输出省略 patch 的版本号（如 "1.15"），这里在输入层统一
// 规范化为 "1.15.0"，避免下游严格 SemVer 解析报错（C18）。
func ParseVersion(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		// 形如: sing-box version 1.14.0
		if len(fields) >= 3 && fields[0] == "sing-box" && fields[1] == "version" {
			return normalizeVersionInput(strings.TrimPrefix(fields[2], "v")), nil
		}
	}
	return "", fmt.Errorf("无法从输出中解析版本: %q", firstLine(output))
}

// normalizeVersionInput 把省略 patch 的版本号（"1.15"）补全为 "1.15.0"，
// 仅对两段纯数字的 core 生效（含 "-pre"/"+build" 后缀时只补 core）；
// 其余输入原样返回，交由 parseSemVersion 严格校验。
func normalizeVersionInput(s string) string {
	core := s
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core = s[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) == 2 && isSemNum(parts[0]) && isSemNum(parts[1]) {
		return core + ".0" + s[len(core):]
	}
	return s
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
