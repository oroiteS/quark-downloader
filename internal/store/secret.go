// secret.go 实现 §6.1 账号 Cookie 加密存储(Task 3):
//
//	优先 go-keyring(service=feiwu、user=cookie)承载平台级用户绑定加密;
//	keyring 不可用(首次 Set 失败,进程内缓存该状态)→ AES-GCM 文件兜底
//	<DataDir>/secret.bin。两处的密文同格式:enc:v1:<base64(nonce||ciphertext)>,
//	加密密钥均为机器绑定(主机名+用户名 SHA-256 → 32 字节)并以应用熵盐
//	feiwu.Downloader.Cookie.v1 参与派生。
//
// 取值语义(§6.1 逐字):读到无前缀值 → 旧版明文直接返回(下次 Save 自动
// 升级加密);enc:v1: → 解密;解密失败(换机器/换用户/损坏) → 返回空串按
// 「未登录」处理,绝不把乱码拿去请求;两处都不存在 → 空串。加密不可用 →
// 降级明文存储(可用性优先)。
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	keyring "github.com/zalando/go-keyring"
)

const (
	// keyringService/keyringUser 是 keyring 条目坐标(service=feiwu、
	// user=cookie,计划逐字)。
	keyringService = "feiwu"
	keyringUser    = "cookie"
	// cookieSalt 应用熵盐(§6.1「固定盐」):参与机器绑定密钥派生,防止
	// 其它程序用默认参数(SHA-256(host+user))解开兜底密文。
	cookieSalt = "feiwu.Downloader.Cookie.v1"
	// encPrefix 密文版本前缀(§6.1「格式版本化」):无前缀 = 旧版明文。
	encPrefix = "enc:v1:"
	// secretFileName 兜底文件名,位于 Paths.DataDir。
	secretFileName = "secret.bin"
	// secretFilePerm 兜底文件权限:秘密数据,严于 settings.json 的 0644。
	secretFilePerm = 0o600
)

// errKeyringNotFound 是包内哨兵:realKeyring 把 go-keyring 的
// ErrNotFound 翻译过来,与具体后端解耦(假 keyring 同样返回它)。
var errKeyringNotFound = errors.New("store: keyring 条目不存在")

// keyringAPI 是 go-keyring 的最小消费面。必须接口化:真 keyring 在
// CI/测试环境不可用,测试注入假实现;生产路径用 realKeyring 适配。
type keyringAPI interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

// realKeyring 适配生产 go-keyring。
type realKeyring struct{}

func (realKeyring) Get(service, user string) (string, error) {
	v, err := keyring.Get(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", errKeyringNotFound
	}
	return v, err
}

func (realKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (realKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

// SecretStore 承载 Cookie 的加密存取。零值不可用,经 NewSecretStore(生产)
// 或 newSecretStore(测试注入)构造。
type SecretStore struct {
	mu    sync.Mutex
	paths Paths
	kr    keyringAPI
	// key 机器绑定密钥(熵盐参与派生),构造时算一次(进程内机器标识稳定)。
	key []byte
	// keyringDown 进程内缓存:keyring 已判定不可用,后续读写直接走文件,
	// 不再撞墙。降级判据(计划逐字):keyring.Set 首次失败;Get 的硬错误
	// (非 NotFound)与 Set 失败同源,一并降级。
	keyringDown bool
}

// NewSecretStore 生产入口:真实 keyring 适配 + 真实机器标识(主机名/
// 用户名取不到时对应分量为空串,密钥仍可派生——可用性优先)。构造期不
// 探测 keyring(探测发生在首次 Set)。
func NewSecretStore(p Paths) *SecretStore {
	hostname, _ := os.Hostname() // 失败 → "",同台机器进程间稳定退化
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	return newSecretStore(p, realKeyring{}, hostname, username)
}

// newSecretStore 是可注入实现体:keyring 实现与机器标识可替换,测试离线。
func newSecretStore(p Paths, kr keyringAPI, hostname, username string) *SecretStore {
	return &SecretStore{
		paths: p,
		kr:    kr,
		key:   deriveKey(cookieSalt, hostname, username),
	}
}

// deriveKey 派生机器绑定密钥:SHA-256(hostname \x00 username \x00 salt)
// → 32 字节(恰为 AES-256)。分量间以 \x00 分隔消除拼接歧义;熵盐收尾
// 参与(§6.1 应用熵)。host/user 任一变化、或换盐,密钥即变。
func deriveKey(salt, hostname, username string) []byte {
	h := sha256.New()
	h.Write([]byte(hostname))
	h.Write([]byte{0})
	h.Write([]byte(username))
	h.Write([]byte{0})
	h.Write([]byte(salt))
	return h.Sum(nil)
}

// secretFile 返回兜底文件路径 <DataDir>/secret.bin。
func (s *SecretStore) secretFile() string {
	return filepath.Join(s.paths.DataDir, secretFileName)
}

// encryptSecret 以 AES-256-GCM 加密:输出 enc:v1:<base64(nonce||ciphertext)>,
// nonce 每次随机 12 字节(同明文两次加密密文不同)。key 非 16/24/32 字节
// 报错(调用方据此降级明文)。
func encryptSecret(key []byte, plain string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// decryptSecret 是 encryptSecret 的逆:剥前缀 → base64 解码 → GCM 开封
// (认证失败即报错,绝不吐出被篡改的明文)。
func decryptSecret(key []byte, enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(enc, encPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("store: 密文长度 %d 短于 nonce %d", len(raw), gcm.NonceSize())
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// SaveCookie 加密落存 cookieStr:优先 keyring(keyring 与文件两处密文
// 同格式,均带 enc:v1 前缀区分明文);keyring 不可用(或首次 Set 失败
// 后永久降级)→ AES-GCM 兜底文件。加密本身不可用(密钥长度异常等极端
// 情形)→ 降级明文写文件(§6.1 可用性优先;无前缀值下次读按旧版明文
// 返回,闭环不破)。
func (s *SecretStore) SaveCookie(cookieStr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	enc, encErr := encryptSecret(s.key, cookieStr)
	if encErr != nil {
		return s.writeSecretFile(cookieStr)
	}
	if !s.keyringDown {
		if err := s.kr.Set(keyringService, keyringUser, enc); err == nil {
			return nil
		}
		s.keyringDown = true // 首次 Set 失败即永久降级文件(进程内缓存)
	}
	return s.writeSecretFile(enc)
}

// LoadCookie 取回 Cookie,无值/坏值一律空串(按未登录):
//
//  1. keyring(未降级时)有条目 → 解码返回;NotFound → 落文件(可能曾以
//     keyring 不可用兜底写入);硬错误 → 降级并落文件;
//  2. 兜底文件 → 解码返回;不存在/不可读 → 空串。
func (s *SecretStore) LoadCookie() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.keyringDown {
		v, err := s.kr.Get(keyringService, keyringUser)
		switch {
		case err == nil:
			return s.decodeCookie(v)
		case errors.Is(err, errKeyringNotFound):
			// keyring 没存过 → 试文件
		default:
			s.keyringDown = true // 后端坏了,与 Set 失败同源,一并降级
		}
	}
	b, err := os.ReadFile(s.secretFile())
	if err != nil {
		return "" // 文件也不存在/不可读 → 未登录
	}
	return s.decodeCookie(strings.TrimSpace(string(b))) // Trim 容忍手编换行
}

// decodeCookie 是 keyring 与文件共用的取值语义(§6.1 逐字):无前缀 →
// 旧版明文直接返回(下次 Save 自动升级加密);enc:v1: → 解密;解密失败
// → 空串 + 一行警告日志(Review Focus #1 红线:绝不返回乱码)。
func (s *SecretStore) decodeCookie(value string) string {
	if !strings.HasPrefix(value, encPrefix) {
		return value
	}
	plain, err := decryptSecret(s.key, value)
	if err != nil {
		log.Printf("store: Cookie 解密失败(换机器/换用户/损坏),按未登录处理: %v", err)
		return ""
	}
	return plain
}

// writeSecretFile 原子写兜底文件(tmp + Sync + rename,沿用 SaveSettings
// 的断电安全模式);DataDir 不存在则先建(建目录是首次写入者的职责,
// 见 paths.go)。
func (s *SecretStore) writeSecretFile(content string) error {
	if err := os.MkdirAll(s.paths.DataDir, 0o755); err != nil {
		return err
	}
	tmp := s.secretFile() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, secretFilePerm)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // 数据先落盘,再换目录项
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.secretFile())
}
