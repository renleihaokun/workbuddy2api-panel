// save_merged_test.go 合并写：登录/导入/重登「只换凭证、不碰手写键」。
//
// 现场（同类项目 issue #210/#187）：按上游响应重建整份文件会把用户手写的
// device_token（设备风控凭据）/ proxy / weight 静默清掉——重登一次，号的定义就变了。
package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v\n%s", err, raw)
	}
	return doc
}

// TestSaveAtomicMergedPreservesUnknownKeys 只覆盖 auth/account，顶层手写键原样保留。
func TestSaveAtomicMergedPreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	seed := `{"auth":{"accessToken":"at-old","refreshToken":"rt-old","expiresAt":1,"domain":"","realm":"cn","token_source":"plugin"},` +
		`"account":{"uid":"u1","nickname":"old","enterpriseId":"e1"},` +
		`"device_token":"dev-keep","proxy":"line-a","weight":3}`
	if err := os.WriteFile(fp, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{UID: "u1", AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: 99,
		Nickname: "new", EnterpriseID: "e2", FilePath: fp, realm: "cn"}

	if err := a.SaveAtomicMerged(); err != nil {
		t.Fatalf("merge save: %v", err)
	}
	doc := readDoc(t, fp)
	for _, k := range []string{"device_token", "proxy", "weight"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("顶层手写键 %q 丢了: %v", k, doc)
		}
	}
	authObj := doc["auth"].(map[string]any)
	if authObj["accessToken"] != "at-new" || authObj["refreshToken"] != "rt-new" {
		t.Errorf("凭证未更新: %v", authObj)
	}
	if authObj["token_source"] != "plugin" {
		t.Errorf("auth 段里本包不认识的键丢了: %v", authObj)
	}
	acct := doc["account"].(map[string]any)
	if acct["nickname"] != "new" || acct["enterpriseId"] != "e2" {
		t.Errorf("account 段未更新: %v", acct)
	}
}

// TestSaveAtomicMergedSyncsDeviceTokenBack 文件里有、内存里没有 → 同步回内存，
// 否则后续 refresh 走 SaveAtomic 又会把它写没（合并写必须闭环的一步）。
func TestSaveAtomicMergedSyncsDeviceTokenBack(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	if err := os.WriteFile(fp, []byte(`{"auth":{"accessToken":"at-old","refreshToken":"rt-old"},`+
		`"account":{"uid":"u1"},"device_token":"dev-keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{UID: "u1", AccessToken: "at-new", RefreshToken: "rt-new", FilePath: fp}
	if err := a.SaveAtomicMerged(); err != nil {
		t.Fatal(err)
	}
	if a.DeviceToken != "dev-keep" {
		t.Fatalf("device_token 应同步回内存，got %q", a.DeviceToken)
	}
	// 后续普通 SaveAtomic（刷新路径）也不该把文件里的 device_token 写没。
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if doc := readDoc(t, fp); doc["device_token"] != "dev-keep" {
		t.Fatalf("SaveAtomic 丢了 device_token: %v", doc)
	}
}

// TestSaveAtomicMergedKeepsFlatShape 扁平形文件按扁平形合并，不擅自改成嵌套形。
func TestSaveAtomicMergedKeepsFlatShape(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u2.json")
	if err := os.WriteFile(fp, []byte(`{"accessToken":"at-old","refreshToken":"rt-old","uid":"u2","realm":"global"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{UID: "u2", AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: 5, FilePath: fp, realm: "global"}
	if err := a.SaveAtomicMerged(); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, fp)
	if _, nested := doc["auth"]; nested {
		t.Fatalf("扁平形文件不该被改成嵌套形: %v", doc)
	}
	if doc["accessToken"] != "at-new" || doc["realm"] != "global" {
		t.Errorf("扁平形字段未更新: %v", doc)
	}
}

// TestSaveAtomicMergedNewFileIsNested 没有旧文件时等价全量写（嵌套形 + device_token）。
func TestSaveAtomicMergedNewFileIsNested(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u3.json")
	a := &Auth{UID: "u3", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 7, FilePath: fp, DeviceToken: "dev"}
	if err := a.SaveAtomicMerged(); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, fp)
	if _, ok := doc["auth"].(map[string]any); !ok {
		t.Fatalf("新文件应为嵌套形: %v", doc)
	}
	if doc["device_token"] != "dev" {
		t.Errorf("device_token 未写入: %v", doc)
	}
}

// TestSaveAtomicMergedRefusesEmptyToken 空 accessToken 拒绝写回（与 SaveAtomic 同护栏）。
func TestSaveAtomicMergedRefusesEmptyToken(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u4.json")
	a := &Auth{UID: "u4", FilePath: fp}
	if err := a.SaveAtomicMerged(); err == nil {
		t.Fatal("空 accessToken 应拒绝写回")
	}
}
