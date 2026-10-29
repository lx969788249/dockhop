package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/helper"
)

// 数据卷的读写都通过临时容器和 Docker 接口完成，不需要 alpine 之类的辅助镜像：
//   - 读、写：创建一个不启动的临时容器挂载这个卷，用“容器文件归档”接口读出或写入；
//   - 清空：接口没有删除文件的功能，所以启动一个临时容器，在里面运行 DockHop 自己
//     （程序是静态编译的，任何镜像里都能运行）来删除卷里的文件。

type volumeIO struct {
	dc        *docker.Client
	image     string // 用来创建临时容器的镜像（任何本地镜像都行）
	helperBin string // 宿主机上 DockHop 程序的路径，清空卷时挂进临时容器
	lock      string // 这个任务拿着的锁，标在临时容器上（见 helper.Label）
	n         atomic.Int32
}

func (v *volumeIO) create(ctx context.Context, volume string, entrypoint []string, readOnly bool) (string, error) {
	hc := map[string]any{
		"NetworkMode": "none",
		"Mounts":      []map[string]any{{"Type": "volume", "Source": volume, "Target": "/dockhop-data", "ReadOnly": readOnly}},
	}
	if v.helperBin != "" && len(entrypoint) > 0 && entrypoint[0] == "/dockhop-helper" {
		hc["Binds"] = []string{v.helperBin + ":/dockhop-helper:ro"}
	}
	spec := map[string]any{
		"Image":      v.image,
		"Entrypoint": entrypoint,
		"User":       "0:0",
		"WorkingDir": "/",
		"Labels":     helper.Labels(v.lock),
		"HostConfig": hc,
	}
	name := fmt.Sprintf("dockhop-helper-%s-%d", runID, v.n.Add(1))
	return v.dc.CreateContainer(ctx, name, spec)
}

func (v *volumeIO) remove(ctx context.Context, id string) {
	_ = v.dc.RemoveContainerAndVolumes(context.WithoutCancel(ctx), id)
}

// read 读出卷里的全部内容（tar，路径以 dockhop-data/ 开头）。
func (v *volumeIO) read(ctx context.Context, volume string) (io.ReadCloser, error) {
	id, err := v.create(ctx, volume, []string{"/dockhop-helper-never-started"}, true)
	if err != nil {
		return nil, err
	}
	rc, err := v.dc.GetArchive(ctx, id, "/dockhop-data")
	if err != nil {
		v.remove(ctx, id)
		return nil, err
	}
	return &closer{ReadCloser: rc, after: func() { v.remove(ctx, id) }}, nil
}

type closer struct {
	io.ReadCloser
	after func()
}

func (c *closer) Close() error {
	err := c.ReadCloser.Close()
	c.after()
	return err
}

// write 把 tar（路径以 dockhop-data/ 开头）写进卷里，保留属主和权限。
func (v *volumeIO) write(ctx context.Context, volume string, r io.Reader) error {
	id, err := v.create(ctx, volume, []string{"/dockhop-helper-never-started"}, false)
	if err != nil {
		return err
	}
	defer v.remove(ctx, id)
	return v.dc.PutArchive(ctx, id, "/", r)
}

// clear 删除卷里的全部内容。
func (v *volumeIO) clear(ctx context.Context, volume string) error {
	if v.helperBin == "" {
		return errors.New("没法清空已有的数据卷：Docker 不在这台服务器上")
	}
	id, err := v.create(ctx, volume, []string{"/dockhop-helper", "__clear", "/dockhop-data"}, false)
	if err != nil {
		return err
	}
	defer v.remove(ctx, id)
	if err := v.dc.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("启动清空数据卷的临时容器失败：%v", err)
	}
	code, err := v.dc.WaitContainer(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("清空数据卷失败（退出码 %d）：%s", code, v.dc.ContainerLogs(ctx, id, 10))
	}
	return nil
}

// replace 把卷的内容换成 r：先清空，再写入。
func (v *volumeIO) replace(ctx context.Context, volume string, r io.Reader) error {
	if err := v.clear(ctx, volume); err != nil {
		return err
	}
	return v.write(ctx, volume, r)
}

// prepareHelper 把 DockHop 程序复制到自己的目录里，供临时容器挂载：通过一行命令
// 运行时，程序在 /tmp 下，有的系统 /tmp 不允许执行程序。
func prepareHelper(home string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(home, "bin", "dockhop-helper")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	in, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return dst, os.Rename(tmp, dst)
}
