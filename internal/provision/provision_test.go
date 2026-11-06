package provision

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestCredentialEntryCodeBuddy(t *testing.T) {
	c := &Credential{
		Provider: "codebuddy", Region: "cn", UID: "u1", Nickname: "主号",
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1700000000,
		Domain: "https://www.codebuddy.cn", EnterpriseID: "ent",
	}
	e := c.Entry()
	if e["provider"] != "codebuddy" {
		t.Fatalf("provider = %v", e["provider"])
	}
	acct, ok := e["account"].(map[string]any)
	if !ok || acct["uid"] != "u1" || acct["enterpriseId"] != "ent" {
		t.Fatalf("account = %#v", e["account"])
	}
	authObj, ok := e["auth"].(map[string]any)
	if !ok || authObj["accessToken"] != "at" || authObj["expiresAt"] != int64(1700000000) {
		t.Fatalf("auth = %#v", e["auth"])
	}
}

func TestCredentialEntryFlat(t *testing.T) {
	c := &Credential{
		Provider: "qoder", UID: "q1", Nickname: "q",
		AccessToken: "dt", RefreshToken: "drt", ExpiresAt: 1700000001,
		Extra: map[string]string{"pat": "pt-x"},
	}
	e := c.Entry()
	if e["uid"] != "q1" || e["accessToken"] != "dt" || e["provider"] != "qoder" {
		t.Fatalf("entry = %#v", e)
	}
	if _, has := e["account"]; has {
		t.Fatal("扁平形不应带 account 段")
	}
	ex, ok := e["extra"].(map[string]any)
	if !ok || ex["pat"] != "pt-x" {
		t.Fatalf("extra = %#v", e["extra"])
	}
	// 空 refreshToken 不应写出这个字段（auth.Parse 会把空串当无 refresh）
	if _, has := (&Credential{Provider: "trae", UID: "t", AccessToken: "x"}).Entry()["refreshToken"]; has {
		t.Fatal("空 refreshToken 不应写入")
	}
}

func TestNextKey(t *testing.T) {
	top := map[string]json.RawMessage{
		"trae_1": {}, "trae_3": {}, "codebuddy_2": {}, "other": {},
	}
	if got := nextKey(top, "trae"); got != "trae_4" {
		t.Fatalf("nextKey(trae) = %q, want trae_4", got)
	}
	if got := nextKey(top, "codebuddy"); got != "codebuddy_3" {
		t.Fatalf("nextKey(codebuddy) = %q", got)
	}
	if got := nextKey(top, "qoder"); got != "qoder_1" {
		t.Fatalf("nextKey(qoder) = %q", got)
	}
}

func TestSaveReusesSlotForSameUID(t *testing.T) {
	af := filepath.Join(t.TempDir(), "auth.json")
	exp := time.Now().Add(time.Hour).Unix()

	first, err := Save(af, "", &Credential{Provider: "deepseek", UID: "u-1", AccessToken: "tok1", ExpiresAt: exp})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if first != "deepseek_1" {
		t.Fatalf("first label = %q", first)
	}
	// 同 uid 重登必须回到原槽位，而不是 deepseek_2：
	// 池按 uid 建 entry，同 uid 两条凭证谁生效不确定（「重登还是 401」的成因）。
	second, err := Save(af, "", &Credential{Provider: "deepseek", UID: "u-1", AccessToken: "tok2", ExpiresAt: exp})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if second != first {
		t.Fatalf("second label = %q, want %q", second, first)
	}
	top, err := loadTop(af)
	if err != nil || len(top) != 1 {
		t.Fatalf("auth.json 应有 1 个槽位，实际 %d（err=%v）", len(top), err)
	}
	var got struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(top[first], &got); err != nil || got.AccessToken != "tok2" {
		t.Fatalf("槽位未覆盖为新凭证: %#v err=%v", got, err)
	}
}

func TestSaveDoubaoFixedSlot(t *testing.T) {
	af := filepath.Join(t.TempDir(), "auth.json")
	exp := time.Now().Add(time.Hour).Unix()
	if label, err := Save(af, "doubao_7", &Credential{Provider: "doubao", UID: "d-1", AccessToken: "s1", ExpiresAt: exp}); err != nil || label != "doubao_1" {
		t.Fatalf("label=%q err=%v（豆包固定单槽位 doubao_1）", label, err)
	}
}

func TestSaveRefusesEmptyToken(t *testing.T) {
	af := filepath.Join(t.TempDir(), "auth.json")
	if _, err := Save(af, "", &Credential{Provider: "deepseek", UID: "u", AccessToken: ""}); err == nil {
		t.Fatal("空 accessToken 必须拒绝写盘")
	}
	if _, err := Save(af, "", &Credential{Provider: "deepseek", UID: "", AccessToken: "x"}); err == nil {
		t.Fatal("空 uid 必须拒绝写盘")
	}
}

func TestAdaptDoubaoCredential(t *testing.T) {
	out := adaptDoubaoCredential(map[string]any{
		"sessionid":               "sid",
		"ttwid":                   "tw",
		"passport_csrf_token_default": "csrf",
	})
	if out["accessToken"] != "sid" {
		t.Fatalf("accessToken = %v（sessionid 应提为 accessToken）", out["accessToken"])
	}
	if out["uid"] != "doubao-work-1" {
		t.Fatalf("uid = %v", out["uid"])
	}
	ex, _ := out["extra"].(map[string]any)
	if ex["ttwid"] != "tw" || ex["passport_csrf_token"] != "csrf" {
		t.Fatalf("extra = %#v", out["extra"])
	}
}

func TestAdaptDeepseekCredential(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		tok  string
		uid  string
	}{
		{"structured", map[string]any{"accessToken": "t1"}, "t1", "deepseek-1"},
		{"localStorage", map[string]any{"userToken": map[string]any{"value": "t2"}}, "t2", "deepseek-1"},
		{"usersCurrent", map[string]any{"token": "t3", "id": "uuid-9"}, "t3", "uuid-9"},
	}
	for _, c := range cases {
		out := adaptDeepseekCredential(c.in)
		if out["accessToken"] != c.tok || out["uid"] != c.uid {
			t.Fatalf("%s: accessToken=%v uid=%v", c.name, out["accessToken"], out["uid"])
		}
	}
}

func TestFromPasteDeepSeekDefaults(t *testing.T) {
	// 不走网络：只验证归一与寿命估算（豆包/DeepSeek 必须写有限 expiresAt，
	// 否则 NeedsRefresh 永假、看护永不续期）。
	cred, err := FromPaste("deepseek", map[string]any{"accessToken": "mb5+abc"}, "我的DS")
	if err != nil {
		t.Fatalf("FromPaste: %v", err)
	}
	if cred.Provider != "deepseek" || cred.UID != "deepseek-1" || cred.Nickname != "我的DS" {
		t.Fatalf("cred = %#v", cred)
	}
	if cred.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiresAt 应为未来的有限值，实际 %d", cred.ExpiresAt)
	}
}

func TestFromPasteRejectsCodeBuddy(t *testing.T) {
	if _, err := FromPaste("codebuddy", map[string]any{"accessToken": "x"}, ""); err == nil {
		t.Fatal("codebuddy 应走设备流，不接受粘贴建号")
	}
}

func TestPickRegion(t *testing.T) {
	if _, r, err := PickRegion(""); err != nil || r.Base != "https://copilot.tencent.com" {
		t.Fatalf("默认应为 cn: %v %#v", err, r)
	}
	if _, r, err := PickRegion("intl"); err != nil || r.Origin != "https://www.codebuddy.ai" {
		t.Fatalf("intl = %#v err=%v", r, err)
	}
	if _, _, err := PickRegion("us"); err == nil {
		t.Fatal("非法区域应报错")
	}
}

// KeyPrefix 决定 auth.json 里的账号标签前缀。
// trae 国内/国际是两套独立体系（域名、认证头、积分接口全不同），
// 标签必须能一眼区分，否则排查时 trae_1 / trae_2 看不出打的是哪家。
// TestFromPasteRegionSetsRegion 防回归：FromPasteRegion 必须把入参 region
// 回填到 Credential.Region，否则 Save 里 KeyPrefix(c.Provider, c.Region) 收到空串，
// 国际版账号会拿到国内前缀（qoder_2 而不是 qoderai_1），
// 标签看不出打的是 gateway.qoder.com.cn 还是 api3.qoder.sh。
//
// 用 doubao 做样本：它的 FromPasteRegion 分支纯本地（不换 token、不打网络），
// 而 region 回填发生在 provider switch **之前**，对所有 provider 一视同仁。
func TestFromPasteRegionSetsRegion(t *testing.T) {
	for _, region := range []string{"", "cn", "intl"} {
		cred, err := FromPasteRegion("doubao", region, map[string]any{"sessionid": "x"}, "t")
		if err != nil {
			t.Fatalf("FromPasteRegion(doubao,%q): %v", region, err)
		}
		if cred.Region != region {
			t.Errorf("Region = %q, want %q（未回填会让 KeyPrefix 拿不到区域）", cred.Region, region)
		}
	}
}

// TestQoderRegionPrefix 区域前缀端到端：qoder intl 必须落到 qoderai_1。
func TestQoderRegionPrefix(t *testing.T) {
	cases := []struct{ region, want string }{
		{"intl", "qoderai_1"},
		{"cn", "qoder_1"},
		{"", "qoder_1"},
	}
	for _, c := range cases {
		cred := &Credential{Provider: "qoder", Region: c.region}
		got := nextKey(map[string]json.RawMessage{}, KeyPrefix(cred.Provider, cred.Region))
		if got != c.want {
			t.Errorf("qoder region=%q → %q, want %q", c.region, got, c.want)
		}
	}
}

func TestKeyPrefix(t *testing.T) {
	cases := []struct{ provider, region, want string }{
		{"codebuddy", "", "codebuddy"},
		{"codebuddy", "cn", "codebuddy"},
		{"codebuddy", "intl", "codebuddyai"},
		{"codebuddy", "INTL", "codebuddyai"},
		{"trae", "", "trae"},
		{"trae", "cn", "trae"},
		{"trae", "intl", "traeai"},
		{"trae", "INTL", "traeai"},
		{"qoder", "", "qoder"},
		{"qoder", "cn", "qoder"},
		// Qoder 国内/国际是两套独立体系（gateway.qoder.com.cn vs api3.qoder.sh），
		// 键前缀必须能区分，否则 qoder_1 / qoder_2 看不出打的是哪家。
		{"qoder", "intl", "qoderai"},
		{"qoder", "INTL", "qoderai"},
		{"doubao", "", "doubao"},
		{"deepseek", "", "deepseek"},
	}
	for _, c := range cases {
		if got := KeyPrefix(c.provider, c.region); got != c.want {
			t.Errorf("KeyPrefix(%q,%q) = %q, want %q", c.provider, c.region, got, c.want)
		}
	}
}
