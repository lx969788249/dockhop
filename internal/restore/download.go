package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lx969788249/dockhop/internal/ui"
)

// 下载迁移包和要从旧服务器拿的镜像文件，用之前按迁移计划核对。下载的文件放在这次迁入的
// 工作目录里：失败时保留，用同一个链接重新迁入不用重新下载；换了新链接，旧的就删掉。

// 核对下载的文件时停下的两种情况（ui.Failure 的 Kind）。
var (
	errNoChecksums = errors.New("迁移计划里没有镜像文件的校验值")
	errMismatch    = errors.New("下载的文件和迁移计划里记的校验值对不上")
)

// dropStale 删掉以前的链接留下的下载和记录。失败时保留它们，是为了用同一个链接重新迁入时
// 不用重新下载、跳过已经迁入的；一个链接对应旧服务器上的一次迁出，换了新链接，旧的就用不上了。
func (r *run) dropStale() {
	keep := "in-" + r.opts.Link.Token
	for _, dir := range []string{filepath.Join(r.Home, "work"), filepath.Join(r.Home, "state")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if name := strings.TrimSuffix(e.Name(), ".json"); strings.HasPrefix(name, "in-") && name != keep {
				_ = os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
}

func (r *run) workBase(chk *checked) string {
	if chk.d.Workdir != "" {
		return chk.d.Workdir
	}
	return filepath.Join(r.Home, "work")
}

// prepareWorkdir 建这次迁入的工作目录（按链接里的下载口令起名：同一个链接重新迁入时接着用上次下载的），
// 记进 pkg。
func (r *run) prepareWorkdir(chk *checked, pkg *bundleFiles) error {
	pkg.workdir = filepath.Join(r.workBase(chk), "in-"+r.opts.Link.Token)
	pkg.dataDir = filepath.Join(pkg.workdir, "data")
	if err := os.MkdirAll(pkg.workdir, 0o700); err != nil {
		return &ui.Failure{Reason: "无法创建工作目录 " + pkg.workdir, Err: err}
	}
	// 上次失败留下的解密数据先删掉，重新解开（加密的下载文件保留，不用重新下载）。
	if err := os.RemoveAll(pkg.dataDir); err != nil {
		return &ui.Failure{Reason: "无法清理上次留下的文件 " + pkg.dataDir, Err: err}
	}
	return nil
}

// download 按检查的结果（chk）下载迁移包和要从旧服务器拿的镜像文件，下到哪里记进 pkg。
func (r *run) download(ctx context.Context, chk *checked, pkg *bundleFiles) error {
	if err := r.checkSums(chk); err != nil {
		return err
	}
	if err := r.prepareWorkdir(chk, pkg); err != nil {
		return err
	}
	pkg.bundlePath = filepath.Join(pkg.workdir, "bundle")
	pkg.downloaded = true
	// 先下载要从旧服务器拿的镜像，最后下载迁移包：迁移包一下完，旧服务器就显示“正在迁入”。
	var parts []fetchPart
	for _, f := range chk.plan.Images {
		if chk.fromOldServer(f) {
			parts = append(parts, fetchPart{url: r.opts.Link.ImageURL(f.File), path: pkg.imagePath(f), size: f.Bytes})
		}
	}
	parts = append(parts, fetchPart{url: r.opts.Link.BundleURL(), path: pkg.bundlePath, size: chk.plan.Sizes.Bundle})
	return r.fetch(ctx, "下载迁移包", parts)
}

// dropDownload 删掉下载的迁移包：解开后发现重新运行也用不上它。
func (pkg *bundleFiles) dropDownload() {
	if pkg.downloaded {
		_ = os.Remove(pkg.bundlePath)
	}
}

// checkSums 确认可能要从旧服务器下载的镜像文件（从镜像仓库拉不下来时也会改从旧服务器下载），
// 迁移计划里都记着 SHA-256（见 verifyDownload）。1.1.6 之前的旧服务器没有记：同一次迁移的
// 镜像文件有好几个，被对调了也发现不了，不能用。迁移包只有一个，没有可以对调的，没记时照常用。
func (r *run) checkSums(chk *checked) error {
	for _, f := range chk.plan.Images {
		if chk.images[f.File].local == "" && f.SHA256 == "" {
			return &ui.Failure{Kind: errNoChecksums, Reason: "旧服务器上的 DockHop 是 1.1.6 之前的版本，迁移计划里没有镜像文件的校验值，" +
				"没法确认下载的镜像没有被调换（还没有下载或改动任何东西）",
				Hint: "在旧服务器上结束这次迁出，重新运行一行命令迁出（会用最新版），再用新的链接迁入"}
		}
	}
	return nil
}

// verifyDownload 在用下载的文件之前，按迁移计划（经过认证）里记的 SHA-256 确认它就是旧服务器
// 上的那个文件：同一次迁移的文件都用同一把密钥加密，网络上的设备把两个文件对调，解密也
// 发现不了。对不上时删掉它，重新运行时会重新下载。没有记录时不检查（见 checkSums）。
func (r *run) verifyDownload(path, want, what string) error {
	if want == "" {
		return nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return &ui.Failure{Reason: "读不了下载的" + what + " " + path, Err: err}
	}
	if got != want {
		_ = os.Remove(path)
		return &ui.Failure{Kind: errMismatch, Reason: "下载的" + what + "和旧服务器上的对不上（可能在网络上被改动过），已删除",
			Hint: "重新运行迁入，会重新下载；如果再次出现，检查两台服务器之间的网络"}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cleanFiles 删除工作文件：成功时全部删除；失败时只删解密后的数据，保留加密的
// 下载文件，重试时不用重新下载。
func (r *run) cleanFiles(pkg *bundleFiles, success bool) {
	if pkg.workdir == "" {
		return
	}
	_ = os.RemoveAll(pkg.dataDir)
	if success {
		if err := os.RemoveAll(pkg.workdir); err != nil {
			r.Screen.Warn("临时文件没能删除，请手动删除：%s", pkg.workdir)
		}
	}
}
