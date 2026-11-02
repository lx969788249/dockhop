package docker

import (
	"strings"
	"testing"
)

// 拉取镜像时按 Docker 逐行输出的 JSON 算出下载了多少、下载和解压合起来有多少进展；
// 遇到错误就停下并返回它。
func TestPullStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"status":"Pulling from library/web","id":"1.25"}`,
		`{"status":"Downloading","id":"a","progressDetail":{"current":100,"total":300}}`,
		`{"status":"Downloading","id":"b","progressDetail":{"current":50,"total":80}}`,
		`{"status":"Downloading","id":"a","progressDetail":{"current":300,"total":300}}`,
		`{"status":"Extracting","id":"a","progressDetail":{"current":200,"total":300}}`,
		`{"status":"Pull complete","id":"a"}`,
	}, "\n")
	var downloaded, activity int64
	if err := pullStream(strings.NewReader(stream), func(d, a int64) { downloaded, activity = d, a }); err != nil {
		t.Fatal(err)
	}
	if downloaded != 350 || activity != 550 {
		t.Errorf("下载 %d、进展 %d，应为 350、550", downloaded, activity)
	}
	err := pullStream(strings.NewReader(`{"status":"Downloading","id":"a","progressDetail":{"current":1}}`+"\n"+
		`{"errorDetail":{"message":"toomanyrequests: 拉取次数到上限了"},"error":"toomanyrequests"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "拉取次数到上限了") {
		t.Errorf("应返回拉取时的错误：%v", err)
	}
}
