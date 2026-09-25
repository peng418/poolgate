package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"poolgate/internal/channel"
)

// 授权成功后落的凭证，必须能被 Load 原样读回来（含机器指纹等渠道附加字段）。
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewCredsStore(dir)
	in := channel.Credential{
		UID:          "u-1",
		Nickname:     "微软",
		AccessToken:  "at-123",
		RefreshToken: "rt-456",
		ExpiresAt:    time.Unix(1790241734, 0),
		Extra: map[string]string{
			"machine_id":    "m1",
			"machine_token": "mt1",
			"machine_type":  "windows",
			"device_id":     "d1",
			"api_host":      "https://api.trae.cn",
		},
	}
	path, err := s.Save(channel.TraeWork, in)
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if filepath.Base(path) != "traework-u-1.json" {
		t.Fatalf("文件名应为 <kind>-<uid>.json，实际 %s", filepath.Base(path))
	}

	// 权限必须是 0600（D5：凭证不进日志、不给人读）。
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("凭证文件权限应为 0600，实际 %o", perm)
	}

	// 落盘形态必须是 wild-work 的嵌套形，字段名是驼峰。
	raw, _ := os.ReadFile(path)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if _, ok := probe["auth"]; !ok {
		t.Fatal("必须是嵌套形（auth/account）")
	}
	if !strings.Contains(string(raw), `"machineId"`) || !strings.Contains(string(raw), `"apiHost"`) {
		t.Fatalf("渠道附加字段应写成驼峰键，实际 %s", raw)
	}

	got, err := s.Load(channel.TraeWork)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("期望读回 1 个账号，实际 %d 个", len(got))
	}
	g := got[0]
	if g.UID != in.UID || g.Nickname != in.Nickname || g.AccessToken != in.AccessToken || g.RefreshToken != in.RefreshToken {
		t.Fatalf("凭证字段未原样读回: %+v", g)
	}
	if g.ExpiresAt.Unix() != in.ExpiresAt.Unix() {
		t.Fatalf("过期时间应保留：期望 %d，实际 %d", in.ExpiresAt.Unix(), g.ExpiresAt.Unix())
	}
	for k, want := range in.Extra {
		if got := g.Extra[k]; got != want {
			t.Fatalf("附加字段 %s 期望 %q，实际 %q", k, want, got)
		}
	}
}

// 空凭证绝不能落盘：会写出一个「看着有账号、实际用不了」的文件。
func TestSaveRejectsIncomplete(t *testing.T) {
	s := NewCredsStore(t.TempDir())
	if _, err := s.Save(channel.QoderCN, channel.Credential{AccessToken: "at"}); err == nil {
		t.Fatal("缺 uid 必须拒绝落盘")
	}
	if _, err := s.Save(channel.QoderCN, channel.Credential{UID: "u1"}); err == nil {
		t.Fatal("缺 accessToken 必须拒绝落盘")
	}
	// 被拒之后目录里不该留下垃圾文件。
	files, _ := filepath.Glob(filepath.Join(s.dir, "*"))
	if len(files) != 0 {
		t.Fatalf("拒绝落盘时不应留下文件，实际 %v", files)
	}
}

// 上游给的 uid 不可信：不能让它带着路径分隔符写到目录外面去。
func TestSaveSanitizesUID(t *testing.T) {
	dir := t.TempDir()
	s := NewCredsStore(dir)
	path, err := s.Save(channel.QoderCN, channel.Credential{UID: "../../etc/passwd", AccessToken: "at"})
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("凭证必须落在凭证目录内，实际 %s", path)
	}
	if strings.Contains(filepath.Base(path), "/") || strings.Contains(filepath.Base(path), "..") {
		t.Fatalf("文件名未清洗: %s", filepath.Base(path))
	}
}

// 单个凭证损坏不阻塞其它账号（迁移期从 wild-work 目录拷来的文件不一定都干净）。
func TestLoadSkipsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewCredsStore(dir)
	if _, err := s.Save(channel.WorkBuddyCN, channel.Credential{UID: "good", AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	// 坏文件：非法 JSON
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 坏文件：合法 JSON 但没有 accessToken
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-empty.json"), []byte(`{"auth":{"accessToken":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(channel.WorkBuddyCN)
	if err != nil {
		t.Fatalf("Load 不应因坏文件报错: %v", err)
	}
	if len(got) != 1 || got[0].UID != "good" {
		t.Fatalf("应只读回那个好账号，实际 %+v", got)
	}
}

// 扁平形（老格式）也要能读 —— 迁移期两种形态都会遇到。
func TestLoadFlatForm(t *testing.T) {
	dir := t.TempDir()
	flat := `{"accessToken":"at-flat","refreshToken":"rt-flat","uid":"u-flat","nickname":"扁平","machineId":"m9"}`
	if err := os.WriteFile(filepath.Join(dir, "qodercn-u-flat.json"), []byte(flat), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewCredsStore(dir).Load(channel.QoderCN)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(got) != 1 || got[0].AccessToken != "at-flat" || got[0].Extra["machine_id"] != "m9" {
		t.Fatalf("扁平形解析错误: %+v", got)
	}
}
