package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// Image 是镜像详情里 DockHop 需要读取的部分。
type Image struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Size        int64    `json:"Size"`
}

// InspectImage 读取镜像详情；ref 可以是名字、名字@digest 或镜像 ID。
func (c *Client) InspectImage(ctx context.Context, ref string) (*Image, error) {
	var img Image
	err := c.Get(ctx, "/images/"+ref+"/json", nil, &img)
	if err != nil {
		return nil, err
	}
	return &img, nil
}

// RemoveImage 删除镜像。
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	return c.Delete(ctx, "/images/"+ref, url.Values{"noprune": {"0"}})
}

// SaveImages 导出镜像（相当于 docker save），返回 tar 流。
func (c *Client) SaveImages(ctx context.Context, refs []string) (io.ReadCloser, error) {
	return c.Stream(ctx, http.MethodGet, "/images/get", url.Values{"names": refs}, nil, "")
}

// InspectNetworkRaw 返回网络的完整配置（原始 JSON）。
func (c *Client) InspectNetworkRaw(ctx context.Context, name string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.Get(ctx, "/networks/"+url.PathEscape(name), nil, &raw)
	return raw, err
}

// InspectVolumeRaw 返回卷的完整配置（原始 JSON）。
func (c *Client) InspectVolumeRaw(ctx context.Context, name string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.Get(ctx, "/volumes/"+url.PathEscape(name), nil, &raw)
	return raw, err
}

// VolumeUsage 返回每个卷占用的字节数；读不到大小的卷（非本地驱动等）值为 -1。
func (c *Client) VolumeUsage(ctx context.Context) (map[string]int64, error) {
	q := url.Values{}
	if c.AtLeast("1.42") {
		q.Set("type", "volume") // 只统计卷，快得多
	}
	var out struct {
		Volumes []struct {
			Name      string `json:"Name"`
			UsageData *struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
	}
	if err := c.Get(ctx, "/system/df", q, &out); err != nil {
		return nil, err
	}
	m := make(map[string]int64, len(out.Volumes))
	for _, v := range out.Volumes {
		size := int64(-1)
		if v.UsageData != nil {
			size = v.UsageData.Size
		}
		m[v.Name] = size
	}
	return m, nil
}
