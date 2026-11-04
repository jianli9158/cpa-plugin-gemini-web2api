package chromeauth

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Mag1cFall/Gemini-Web2API/internal/auth"
	"github.com/Mag1cFall/Gemini-Web2API/internal/gemini"
	"github.com/Mag1cFall/Gemini-Web2API/internal/proxypool"
)

const (
	defaultImportLanguage = "en-US,en;q=0.9"
	storageFileName       = "storage-state.json"
)

var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// ImportResult 描述单个账号导入结果
type ImportResult struct {
	Profile     string `json:"profile"`
	Email       string `json:"email"`
	Imported    bool   `json:"imported"`
	CookieCount int    `json:"cookieCount"`
	Path        string `json:"path"`
	CompletedAt string `json:"completedAt"`
}

type pendingImport struct {
	state        auth.StorageState
	result       ImportResult
	requestProxy string
}

// CookieImportOptions 保存 Cookie 导入参数，Cookies 为 Cookie Header、浏览器导出的 JSON 或 Netscape cookies.txt
type CookieImportOptions struct {
	Cookies      string
	ID           string
	Output       string
	Proxy        string
	RequestProxy string
	AuthUser     int
}

// ImportCookies 从 Cookie 文本生成短期账号状态
func ImportCookies(ctx context.Context, options CookieImportOptions) (ImportResult, int, error) {
	proxyURL, requestProxy, err := importProxies(options.Proxy, options.RequestProxy)
	if err != nil {
		return ImportResult{}, 0, err
	}
	id := AccountID(options.ID)
	if id == "" {
		return ImportResult{}, 0, fmt.Errorf("id 必须包含英文字母或数字")
	}
	if options.AuthUser < 0 {
		return ImportResult{}, 0, fmt.Errorf("authuser 不能为负数")
	}
	cookies, err := parseCookies(options.Cookies)
	if err != nil {
		return ImportResult{}, 0, err
	}
	path := filepath.Join(options.Output, id, storageFileName)
	state := auth.StorageState{
		Cookies: cookies,
		Origins: []json.RawMessage{},
		Metadata: auth.Metadata{
			ID:       id,
			Proxy:    proxyURL,
			Source:   auth.ImportSource{Browser: "cookie-header"},
			Language: defaultImportLanguage,
			AuthUser: options.AuthUser,
		},
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return ImportResult{}, 0, fmt.Errorf("解析认证状态路径: %w", err)
	}
	pending := pendingImport{
		state: state,
		result: ImportResult{
			Profile:     "cookie-header",
			Imported:    true,
			CookieCount: len(cookies),
			Path:        absPath,
		},
		requestProxy: requestProxy,
	}
	results, modelCount, err := validateAndWriteImports(ctx, []pendingImport{pending})
	if err != nil {
		return ImportResult{}, 0, err
	}
	return results[0], modelCount, nil
}

func importProxies(proxy string, fallback string) (string, string, error) {
	proxy = strings.TrimSpace(proxy)
	if proxy == proxypool.Direct {
		return proxy, "", nil
	}
	fixed, err := ValidateProxy(proxy)
	if err != nil {
		return "", "", err
	}
	return fixed, cmp.Or(fixed, fallback), nil
}

// ValidateProxy 校验 HTTP、HTTPS、SOCKS4 或 SOCKS5 代理 URL 并返回去掉空白的值
func ValidateProxy(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("proxy 必须是 http、https、socks4、socks4a、socks5 或 socks5h URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
		return value, nil
	default:
		return "", fmt.Errorf("proxy 必须是 http、https、socks4、socks4a、socks5 或 socks5h URL")
	}
}

func validateAndWriteImports(ctx context.Context, pending []pendingImport) ([]ImportResult, int, error) {
	models := make(map[string]struct{})
	for index := range pending {
		item := &pending[index]
		source := gemini.AccountSource{
			ID:       item.state.Metadata.ID,
			Cookies:  stateCookiesToGemini(item.state.Cookies),
			Language: item.state.Metadata.Language,
			AuthUser: item.state.Metadata.AuthUser,
		}
		source.Save = func(cookies []gemini.Cookie) error {
			item.state.Cookies = geminiCookiesToState(cookies)
			return nil
		}
		client, err := gemini.NewClient(source, item.requestProxy, false)
		if err != nil {
			return nil, 0, fmt.Errorf("验证账号 %s: %w", item.state.Metadata.ID, err)
		}
		accountContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = client.Init(accountContext)
		if err == nil {
			_, err = client.FetchUsage(accountContext)
		}
		cancel()
		client.CloseIdleConnections()
		if err != nil {
			return nil, 0, fmt.Errorf("验证账号 %s: %w", item.state.Metadata.ID, err)
		}
		for _, model := range client.Models() {
			models[model.ID] = struct{}{}
		}
	}
	results := make([]ImportResult, len(pending))
	completedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for index := range pending {
		file, err := auth.New(pending[index].result.Path, pending[index].state)
		if err != nil {
			return nil, 0, err
		}
		if err := file.Write(); err != nil {
			return nil, 0, err
		}
		pending[index].result.CompletedAt = completedAt
		results[index] = pending[index].result
	}
	return results, len(models), nil
}

// AccountID 返回邮箱对应的账号 ID
func AccountID(email string) string {
	return strings.Trim(slugPattern.ReplaceAllString(strings.ToLower(strings.TrimSpace(email)), "-"), "-")
}

func stateCookiesToGemini(cookies []auth.StateCookie) []gemini.Cookie {
	result := make([]gemini.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		result = append(result, gemini.Cookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Expires:  int64(cookie.Expires),
			HTTPOnly: cookie.HTTPOnly,
			Secure:   cookie.Secure,
			SameSite: cookie.SameSite,
		})
	}
	return result
}

func geminiCookiesToState(cookies []gemini.Cookie) []auth.StateCookie {
	result := make([]auth.StateCookie, 0, len(cookies))
	for _, cookie := range cookies {
		result = append(result, auth.StateCookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Expires:  float64(cookie.Expires),
			HTTPOnly: cookie.HTTPOnly,
			Secure:   cookie.Secure,
			SameSite: cookie.SameSite,
		})
	}
	return result
}

func parseCookieHeader(header string) ([]auth.StateCookie, error) {
	header = strings.TrimSpace(header)
	if len(header) >= len("Cookie:") && strings.EqualFold(header[:len("Cookie:")], "Cookie:") {
		header = strings.TrimSpace(header[len("Cookie:"):])
	}
	if header == "" || strings.ContainsAny(header, "\r\n") {
		return nil, fmt.Errorf("cookie 必须是一行 Cookie Header")
	}
	cookies := make([]auth.StateCookie, 0)
	names := make(map[string]struct{})
	for _, item := range strings.Split(header, ";") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return nil, fmt.Errorf("Cookie Header 项 %q 格式无效", strings.TrimSpace(item))
		}
		name := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if value == "" {
			return nil, fmt.Errorf("Cookie %s 的值为空", name)
		}
		if _, exists := names[name]; exists {
			continue
		}
		names[name] = struct{}{}
		domain := ".google.com"
		if strings.HasPrefix(name, "__Host-") {
			domain = "gemini.google.com"
		}
		cookies = append(cookies, auth.StateCookie{
			Name:   name,
			Value:  value,
			Domain: domain,
			Path:   "/",
			Secure: true,
		})
	}
	return cookies, nil
}
