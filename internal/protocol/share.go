// share.go 解析用户粘贴的分享文案(纯链接 / 整段文案 / 夸克口令),规格 §2.7。
package protocol

import (
	"errors"
	"regexp"
)

// ShareRef 是一段分享文案解析出的引用信息。
type ShareRef struct {
	PwdID     string // 分享 ID,即 /s/ 后面的部分
	Passcode  string // 提取码,无码分享为空串
	DirFID    string // 起始目录 fid,空串表示从分享根目录开始
	IsKouling bool   // 是否为夸克口令(无公开解析接口,只能提示用户改用分享链接)
}

var (
	// 分享 ID:/s/ 后面的 [A-Za-z0-9_-] 串。
	sharePwdIDRe = regexp.MustCompile(`/s/([A-Za-z0-9_-]+)`)
	// 提取码第一种形态:URL 查询参数 pwd=xxx。
	sharePwdParamRe = regexp.MustCompile(`pwd=([A-Za-z0-9]+)`)
	// 提取码第二种形态:中文关键词引导。
	//
	// 规格原文使用负向后顾 (?<![/A-Za-z0-9]),Go regexp 是 RE2 不支持,
	// 改写为消费一个前导字符:(?:^|[^/A-Za-z0-9])——关键词要么在文本
	// 开头,要么前一个字符不是 / 字母数字(如 /s/pwd1234 里的 pwd 紧贴
	// /s/ 的 /,前导分支不满足,天然挡住)。
	// 命中后再做第二层检查:捕获组 1 前一字符若属 [/A-Za-z0-9],说明是
	// " pwd1234" 这类独立出现的 ID 本体(前导是空格,前导分支挡不住,
	// 但捕获组紧贴 pwd 的 d),跳过该匹配。防线语义与规格一致。
	sharePasscodeRe = regexp.MustCompile(
		`(?:^|[^/A-Za-z0-9])(?:提取码|提取碼|密码|密碼|访问码|訪問碼|口令|pwd|PWD|Pwd)\s*[=:：]?\s*([A-Za-z0-9]{1,12})`)
	// 夸克口令:/~xxxx~ 形态,没有公开解析接口,只做识别标记。
	shareKoulingRe = regexp.MustCompile(`/~([A-Za-z0-9]{4,32})~:?/?`)
	// 目录 fid:/ 后恰好 32 位的字母数字串,可带 "-名称" 后缀。
	shareDirFIDRe = regexp.MustCompile(`/([A-Za-z0-9]{32})(?:-[^/?#]*)?`)
)

// errNoShareLink 是既无分享 ID 也无口令时的错误。
var errNoShareLink = errors.New("未识别到分享链接")

// ParseShareText 从用户粘贴的任意文案中提取分享信息,纯函数、无网络。
// 依次提取:分享 ID → 提取码(pwd= 参数优先,其次中文关键词)→ 口令标记 →
// 目录 fid(取最后一个不等于 pwd_id 的匹配)。既无 pwd_id 也无口令时报错。
func ParseShareText(text string) (ShareRef, error) {
	var ref ShareRef

	if m := sharePwdIDRe.FindStringSubmatch(text); m != nil {
		ref.PwdID = m[1]
	}

	ref.Passcode = extractPasscode(text)

	if shareKoulingRe.MatchString(text) {
		ref.IsKouling = true
	}

	ref.DirFID = extractDirFID(text, ref.PwdID)

	if ref.PwdID == "" && !ref.IsKouling {
		return ShareRef{}, errNoShareLink
	}
	return ref, nil
}

// extractPasscode 提取提取码:先找 pwd= 查询参数,未命中再按中文关键词
// 形态逐个检查,返回第一个通过防线的匹配;都没有则返回空串。
func extractPasscode(text string) string {
	if m := sharePwdParamRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	for _, loc := range sharePasscodeRe.FindAllStringSubmatchIndex(text, -1) {
		// loc[2]:loc[3] 是捕获组 1(提取码本体)的绝对偏移。
		start := loc[2]
		if start > 0 && isShareBoundaryByte(text[start-1]) {
			// 捕获组紧贴 / 字母数字(如 " pwd1234" 里的 1234 紧贴
			// pwd 的 d),是 ID 本体的一部分,不是提取码,跳过。
			continue
		}
		return text[loc[2]:loc[3]]
	}
	return ""
}

// extractDirFID 取最后一个不等于 pwd_id 的 32 位匹配作起始目录。
func extractDirFID(text, pwdID string) string {
	dirFID := ""
	for _, m := range shareDirFIDRe.FindAllStringSubmatch(text, -1) {
		if m[1] != pwdID {
			dirFID = m[1]
		}
	}
	return dirFID
}

// isShareBoundaryByte 判断字节是否属于 [/A-Za-z0-9]。提取码关键词或捕获组
// 的前一字节在此集合内时,说明紧贴的是 ID/URL 本体。中文等多字节字符的
// UTF-8 尾字节 >= 0x80,不会误入本集合。
func isShareBoundaryByte(b byte) bool {
	return b == '/' ||
		(b >= '0' && b <= '9') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z')
}
