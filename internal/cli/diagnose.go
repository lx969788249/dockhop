package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/logging"
)

// runDiagnose 生成诊断文件，写在当前目录；当前目录不可写时写到 DOCKHOP_HOME。
func runDiagnose(env *Env) error {
	screen := env.screen
	now := time.Now()
	in := logging.DiagnoseInput{
		Version:  env.Version,
		Platform: env.Platform,
		Home:     env.dockhopHome(),
		Now:      now,
	}
	screen.Println("正在收集诊断信息……")
	in.Docker, in.DockerErr = docker.New(env.Context)

	name := "dockhop-diagnose-" + now.Format("20060102-150405") + ".txt"
	dirs := []string{env.Workdir, in.Home}
	var f *os.File
	var err error
	for _, d := range dirs {
		if d == "" {
			continue
		}
		_ = os.MkdirAll(d, 0o700)
		f, err = os.OpenFile(filepath.Join(d, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
	}
	if f == nil {
		return fmt.Errorf("无法创建诊断文件：%v", err)
	}
	defer f.Close()
	if err := logging.WriteDiagnose(env.Context, f, in, &logging.Redactor{}); err != nil {
		return fmt.Errorf("写入诊断文件失败：%v", err)
	}
	screen.Println("诊断文件：" + f.Name())
	screen.Println("密钥和密码已自动隐藏，可以直接附到 Issue 里：https://github.com/lx969788249/dockhop/issues")
	return nil
}
