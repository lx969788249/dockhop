package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 下载迁移包和要从旧服务器拿的镜像文件，用之前按迁移计划核对。下载的文件放在这次迁入的
// 工作目录里：失败时保留，用同一个链接重新迁入不用重新下载；换了新链接，旧的就删掉。

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

func (r *run) workBase() string {
	if r.d.Workdir != "" {
		return r.d.Workdir
	}
	return filepath.Join(r.Home, "work")
}

func (r *run) prepareWorkdir(token string) error {
	if token == "" {
		token = r.opts.Link.Token
	}
	r.workdir = filepath.Join(r.workBase(), "in-"+token)
	r.dataDir = filepath.Join(r.workdir, "data")
	if err := os.MkdirAll(r.workdir, 0o700); err != nil {
		return r.Screen.StepFailed("无法创建工作目录 "+r.workdir, err.Error(), "")
	}
	// 上次失败留下的解密数据先删掉，重新解开（加密的下载文件保留，不用重新下载）。
	if err := os.RemoveAll(r.dataDir); err != nil {
		return r.Screen.StepFailed("无法清理上次留下的文件 "+r.dataDir, err.Error(), "")
	}
	return nil
}

func (r *run) download(ctx context.Context) error {
	if err := r.prepareWorkdir(""); err != nil {
		return err
	}
	r.bundlePath = filepath.Join(r.workdir, "bundle")
	r.downloaded = true
	// 先下载要从旧服务器拿的镜像，最后下载迁移包：迁移包一下完，旧服务器就显示“正在迁入”。
	var parts []fetchPart
	for _, f := range r.plan.Images {
		if r.fromOldServer(f) {
			parts = append(parts, fetchPart{url: r.opts.Link.ImageURL(f.File), path: r.imagePath(f), size: f.Bytes})
		}
	}
	parts = append(parts, fetchPart{url: r.opts.Link.BundleURL(), path: r.bundlePath, size: r.plan.Sizes.Bundle})
	return r.fetch(ctx, "下载迁移包", parts)
}

// dropDownload 删掉下载的迁移包：解开后发现重新运行也用不上它。
func (r *run) dropDownload() {
	if r.downloaded {
		_ = os.Remove(r.bundlePath)
	}
}

// verifyDownload 在用下载的文件之前，按迁移计划（经过认证）里记的 SHA-256 确认它就是旧服务器
// 上的那个文件：同一次迁移的文件都用同一把密钥加密，网络上的设备把两个文件对调，解密也
// 发现不了。对不上时删掉它，重新运行时会重新下载。旧版本的迁移计划没有记录时不检查。
func (r *run) verifyDownload(path, want, what string) error {
	if want == "" {
		return nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return r.Screen.StepFailed("读不了下载的"+what+" "+path, err.Error(), "")
	}
	if got != want {
		_ = os.Remove(path)
		return r.Screen.StepFailed("下载的"+what+"和旧服务器上的对不上（可能在网络上被改动过），已删除", "",
			"重新运行迁入，会重新下载；如果再次出现，检查两台服务器之间的网络")
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
func (r *run) cleanFiles(success bool) {
	if r.workdir == "" {
		return
	}
	_ = os.RemoveAll(r.dataDir)
	if success {
		if err := os.RemoveAll(r.workdir); err != nil {
			r.Screen.Warn("临时文件没能删除，请手动删除：%s", r.workdir)
		}
	}
}
