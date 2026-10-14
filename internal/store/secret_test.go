package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeKeyring 是 keyringAPI 的假实现(测试全离线——真 keyring 在 CI/测试
// 环境不可用,计划要求接口化注入假实现)。
type fakeKeyring struct {
	mu       sync.Mutex
	data     map[string]string // service+"/"+user → 值
	setErr   error             // 非 nil → Set 恒失败(模拟 keyring 不可用)
	getErr   error             // 非 nil → Get 恒失败(模拟后端硬错误,非 NotFound)
	setCalls int
	getCalls int
}

func (f *fakeKeyring) Get(service, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.data[service+"/"+user]
	if !ok {
		return "", errKeyringNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Set(service, user, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	if f.data == nil {
		f.data = make(map[string]string)
	}
	f.data[service+"/"+user] = password
	return nil
}

func (f *fakeKeyring) Delete(service, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, service+"/"+user)
	return nil
}

// newTestSecretStore 用假 keyring + 固定机器标识(hostname/username 注入,
// 与生产 NewSecretStore 的真实取值解耦)构造 SecretStore。
func newTestSecretStore(t *testing.T, kr keyringAPI) *SecretStore {
	t.Helper()
	return newSecretStore(testPaths(t.TempDir()), kr, "testhost", "tester")
}

// TestSecretStorePortableSkipsKeyring（Windows portable 打包配套，2025）：
// Portable=true 的 Paths 下 SaveCookie/LoadCookie 完全不触碰 keyring——
// keyring 在 Windows 即凭据管理器（按用户配置存储，不随目录走），portable
// 语义要求数据（含 Cookie）全落 exe 旁 ./data/，换机/换目录随文件夹带走。
// 落点为文件兜底 secret.bin（AES-GCM 机器绑定加密）。
func TestSecretStorePortableSkipsKeyring(t *testing.T) {
	p := testPaths(t.TempDir())
	p.Portable = true
	kr := &fakeKeyring{}
	s := newSecretStore(p, kr, "testhost", "tester")

	if err := s.SaveCookie("puus=portable"); err != nil {
		t.Fatalf("SaveCookie: %v", err)
	}
	if kr.setCalls != 0 || kr.getCalls != 0 {
		t.Fatalf("portable 模式不得触碰 keyring: set=%d get=%d", kr.setCalls, kr.getCalls)
	}
	// 落点必须是 DataDir/secret.bin（keyring 未存任何条目）
	if _, err := os.Stat(secretFilePath(s)); err != nil {
		t.Fatalf("Cookie 未落兜底文件 %s: %v", secretFilePath(s), err)
	}
	// RoundTrip：同机读回明文
	if got := s.LoadCookie(); got != "puus=portable" {
		t.Fatalf("LoadCookie = %q, want %q", got, "puus=portable")
	}
	if kr.getCalls != 0 {
		t.Fatalf("LoadCookie 仍触碰了 keyring: %d 次", kr.getCalls)
	}
}

// secretFilePath 取 s 的兜底文件路径(DataDir/secret.bin)。
func secretFilePath(s *SecretStore) string {
	return filepath.Join(s.paths.DataDir, secretFileName)
}

// TestSecretStoreFileFallbackRoundTrip 文件兜底往返:keyring 恒不可用 →
// 首次 Set 失败即永久降级(进程内缓存,后续 Save 不再撞 keyring);密文落
// <DataDir>/secret.bin,enc:v1: 前缀、base64 解码后不含明文;Load 往返一致。
func TestSecretStoreFileFallbackRoundTrip(t *testing.T) {
	kr := &fakeKeyring{setErr: errors.New("keyring unavailable")}
	s := newTestSecretStore(t, kr)

	if err := s.SaveCookie("__puus=s1; __kp=s2"); err != nil {
		t.Fatalf("SaveCookie(keyring 不可用兜底文件) 出错: %v", err)
	}
	if err := s.SaveCookie("__puus=s3"); err != nil {
		t.Fatalf("第二次 SaveCookie 出错: %v", err)
	}
	if kr.setCalls != 1 {
		t.Errorf("首次 Set 失败即永久降级(进程内缓存),Set 实际调用 %d 次,want 1", kr.setCalls)
	}

	if got := s.LoadCookie(); got != "__puus=s3" {
		t.Errorf("LoadCookie() = %q, want %q(文件兜底往返)", got, "__puus=s3")
	}

	b, err := os.ReadFile(secretFilePath(s))
	if err != nil {
		t.Fatalf("兜底文件未落盘: %v", err)
	}
	if !strings.HasPrefix(string(b), encPrefix) {
		t.Errorf("兜底文件须 %q 前缀,got %.40q", encPrefix, string(b))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(b), encPrefix))
	if err != nil {
		t.Fatalf("前缀后须为合法 base64: %v", err)
	}
	if bytes.Contains(raw, []byte("__puus=s3")) {
		t.Errorf("兜底文件必须是密文,明文泄漏: %q", raw)
	}
}

// TestSecretStoreKeyringRoundTrip keyring 可用路径:密文写 keyring
// (service=feiwu、user=cookie),值同样带 enc:v1: 前缀区分;keyring 成功时
// 不写兜底文件。
func TestSecretStoreKeyringRoundTrip(t *testing.T) {
	kr := &fakeKeyring{}
	s := newTestSecretStore(t, kr)

	if err := s.SaveCookie("__puus=k1"); err != nil {
		t.Fatalf("SaveCookie(keyring) 出错: %v", err)
	}
	v, ok := kr.data[keyringService+"/"+keyringUser]
	if !ok {
		t.Fatalf("keyring 未落条目(service=%q user=%q)", keyringService, keyringUser)
	}
	if !strings.HasPrefix(v, encPrefix) {
		t.Errorf("keyring 值也须 %q 前缀区分,got %.40q", encPrefix, v)
	}
	if got := s.LoadCookie(); got != "__puus=k1" {
		t.Errorf("LoadCookie() = %q, want %q(keyring 往返)", got, "__puus=k1")
	}
	if _, err := os.Stat(secretFilePath(s)); !os.IsNotExist(err) {
		t.Errorf("keyring 可用时不应写兜底文件 secret.bin(err=%v)", err)
	}
}

// TestLoadCookieLegacyPlaintextMigration enc:v1 迁移(§6.1 逐字):存量
// 无前缀明文 → Load 直接返回原文;下次 Save 自动升级为 enc:v1 密文。
func TestLoadCookieLegacyPlaintextMigration(t *testing.T) {
	kr := &fakeKeyring{setErr: errors.New("keyring unavailable")} // 逼走文件,才能钉文件升级
	s := newTestSecretStore(t, kr)

	if err := os.WriteFile(secretFilePath(s), []byte("__puus=legacy-plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadCookie(); got != "__puus=legacy-plain" {
		t.Errorf("旧版明文应直接返回,got %q", got)
	}

	if err := s.SaveCookie("__puus=fresh"); err != nil {
		t.Fatalf("SaveCookie 出错: %v", err)
	}
	b, err := os.ReadFile(secretFilePath(s))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), encPrefix) {
		t.Errorf("Save 后应自动升级为 %q 密文,got %.40q", encPrefix, string(b))
	}
	if got := s.LoadCookie(); got != "__puus=fresh" {
		t.Errorf("升级后 LoadCookie() = %q, want %q", got, "__puus=fresh")
	}
}

// TestLoadCookieCorruptedCiphertext Review Focus #1 红线:坏密文(篡改/
// 非法 base64/空/截断)→ 一律空串按未登录,绝不把乱码拿去请求。
func TestLoadCookieCorruptedCiphertext(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"篡改密文字节", ""}, // 下面动态生成
		{"非法base64", encPrefix + "!!!not-base64!!!"},
		{"空前缀", encPrefix},
		{"截断", encPrefix + base64.StdEncoding.EncodeToString([]byte("short"))},
	}
	// 生成"篡改密文字节":真实 Save 得密文,翻尾字节(GCM 认证标签必炸)。
	kr := &fakeKeyring{setErr: errors.New("keyring unavailable")}
	s := newTestSecretStore(t, kr)
	if err := s.SaveCookie("__puus=will-corrupt"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(secretFilePath(s))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(b), encPrefix))
	if err != nil || len(raw) == 0 {
		t.Fatalf("密文基线生成失败: %v", err)
	}
	raw[len(raw)-1] ^= 0xFF
	cases[0].val = encPrefix + base64.StdEncoding.EncodeToString(raw)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSecretStore(t, &fakeKeyring{}) // keyring 空(NotFound)→ 走文件
			if err := os.WriteFile(secretFilePath(s), []byte(tc.val), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := s.LoadCookie(); got != "" {
				t.Errorf("坏密文必须返回空串(按未登录),got %q", got)
			}
		})
	}
}

// TestLoadCookieMachineChanged 机器绑定:换主机名/用户名 → 派生密钥不同 →
// 解密失败 → 空串(§6.1"换机器/换用户"分支)。
func TestLoadCookieMachineChanged(t *testing.T) {
	dir := t.TempDir()
	p := testPaths(dir)
	down := errors.New("keyring unavailable")

	a := newSecretStore(p, &fakeKeyring{setErr: down}, "hostA", "userA")
	if err := a.SaveCookie("__puus=machine-a"); err != nil {
		t.Fatal(err)
	}
	if got := a.LoadCookie(); got != "__puus=machine-a" {
		t.Errorf("同机器应读回,got %q", got)
	}

	b := newSecretStore(p, &fakeKeyring{setErr: down}, "hostB", "userB")
	if got := b.LoadCookie(); got != "" {
		t.Errorf("换机器解密失败应空串(未登录),got %q", got)
	}
}

// TestLoadCookieAbsent keyring 与文件都不存在 → 空串(未登录)。
func TestLoadCookieAbsent(t *testing.T) {
	s := newTestSecretStore(t, &fakeKeyring{})
	if got := s.LoadCookie(); got != "" {
		t.Errorf("无任何存量时应空串,got %q", got)
	}
}

// TestLoadCookieKeyringGetHardErrorFallsToFile keyring Get 硬错误(非
// NotFound,后端坏了)→ 本次即落文件读取,且降级状态缓存(只撞一次)。
func TestLoadCookieKeyringGetHardErrorFallsToFile(t *testing.T) {
	dir := t.TempDir()
	p := testPaths(dir)
	down := errors.New("keyring unavailable")

	seed := newSecretStore(p, &fakeKeyring{setErr: down}, "testhost", "tester")
	if err := seed.SaveCookie("__puus=file-only"); err != nil {
		t.Fatal(err)
	}

	kr := &fakeKeyring{getErr: errors.New("dbus daemon dead")}
	s := newSecretStore(p, kr, "testhost", "tester")
	if got := s.LoadCookie(); got != "__puus=file-only" {
		t.Errorf("Get 硬错误应落文件兜底,got %q", got)
	}
	if got := s.LoadCookie(); got != "__puus=file-only" {
		t.Errorf("降级缓存后第二次 Load 仍应读文件,got %q", got)
	}
	if kr.getCalls != 1 {
		t.Errorf("Get 硬错误应缓存降级状态,实际调用 %d 次,want 1", kr.getCalls)
	}
}

// TestDeriveKeyMachineBound 密钥派生:32 字节;随 hostname/username 变;
// 熵盐参与派生(§6.1 防其它程序用默认参数解开)。
func TestDeriveKeyMachineBound(t *testing.T) {
	k1 := deriveKey(cookieSalt, "h1", "u1")
	k2 := deriveKey(cookieSalt, "h2", "u2")
	k3 := deriveKey("other.Salt.v9", "h1", "u1")
	if len(k1) != 32 {
		t.Errorf("派生密钥长度 %d, want 32(AES-256)", len(k1))
	}
	if bytes.Equal(k1, k2) {
		t.Errorf("密钥须随机器标识(hostname+username)变化")
	}
	if bytes.Equal(k1, k3) {
		t.Errorf("熵盐须参与派生(换盐 → 换钥)")
	}
}

// TestEncryptSecretRoundTripAndNonce 加解密函数级:往返一致;同明文两次
// 加密 nonce 不同 → 密文不同;换钥解密失败;非法 key 报错(降级路径依据)。
func TestEncryptSecretRoundTripAndNonce(t *testing.T) {
	key := deriveKey(cookieSalt, "h", "u")
	enc1, err := encryptSecret(key, "cookie-value")
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := encryptSecret(key, "cookie-value")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc1, encPrefix) {
		t.Errorf("密文须 %q 前缀,got %q", encPrefix, enc1)
	}
	if enc1 == enc2 {
		t.Errorf("nonce 须唯一:两次加密产生了相同密文")
	}
	for _, e := range []string{enc1, enc2} {
		if got, err := decryptSecret(key, e); err != nil || got != "cookie-value" {
			t.Errorf("decryptSecret(%q) = (%q,%v), want (%q,nil)", e, got, err, "cookie-value")
		}
	}
	other := deriveKey(cookieSalt, "h2", "u2")
	if _, err := decryptSecret(other, enc1); err == nil {
		t.Errorf("换钥解密应失败(GCM 认证)")
	}
	if _, err := encryptSecret(nil, "x"); err == nil {
		t.Errorf("非法 key 应报错(加密不可用 → 降级明文的依据)")
	}
}

// TestNewSecretStore 生产入口 wiring:真实 keyring 适配 + 真实机器标识,
// 构造期不探测 keyring(探测发生在首次 Set,计划语义)。
func TestNewSecretStore(t *testing.T) {
	s := NewSecretStore(testPaths(t.TempDir()))
	if s == nil {
		t.Fatal("NewSecretStore() = nil")
	}
	if len(s.key) != 32 {
		t.Errorf("生产构造的派生密钥长度 %d, want 32", len(s.key))
	}
	if _, ok := s.kr.(realKeyring); !ok {
		t.Errorf("生产入口应绑定 realKeyring,got %T", s.kr)
	}
}
