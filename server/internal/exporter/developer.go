package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errDeveloperLogin  = errors.New("开发后台登录已失效，请重新授权浏览器")
	errDeveloperDenied = errors.New("目标应用返回 403，没有访问权限")
)

type developerLoginVersion struct {
	generation uint64
}

func (e developerLoginVersion) Error() string { return errDeveloperLogin.Error() }
func (e developerLoginVersion) Is(target error) bool {
	return target == errDeveloperLogin
}

type browserAuthorization struct {
	Origin  string
	Headers map[string]string
	Cookies []struct {
		Name, Value, Domain, Path string
		Secure                    bool
		Expires                   float64
	}
}

type developerSession struct {
	mu         sync.Mutex
	service    *Service
	connection Connection
	client     *http.Client
	origin     string
	headers    http.Header
	refreshed  bool
	generation uint64
}

func (s *developerSession) authenticate(ctx context.Context, refresh bool) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	raw, err := s.service.browser.Call(ctx, "authorization", s.connection, map[string]bool{"refresh": refresh})
	if err != nil {
		return err
	}
	var auth browserAuthorization
	if err := json.Unmarshal(raw, &auth); err != nil {
		return errors.New("浏览器助手返回无效的开发后台授权")
	}
	dev, err := trustedURL(auth.Origin)
	if err != nil || !strings.HasSuffix(dev.Hostname(), ".aedev.feishuapp.cn") ||
		!strings.HasPrefix(dev.Hostname(), s.connection.Tenant+"-") || auth.Origin != dev.Scheme+"://"+dev.Host ||
		(s.connection.DevURL != "" && auth.Origin != s.connection.DevURL) || (s.origin != "" && auth.Origin != s.origin) {
		return errors.New("浏览器授权域名与当前连接不一致")
	}

	headers := make(http.Header, len(auth.Headers))
	for name, value := range auth.Headers {
		switch strings.ToLower(name) {
		case "host", "cookie", "content-length", "accept-encoding", "connection", "keep-alive",
			"transfer-encoding", "proxy-authorization", "te", "trailer", "upgrade", "x-svc-method":
			continue
		}
		if !strings.HasPrefix(name, ":") {
			headers.Set(name, value)
		}
	}
	if headers.Get("X-Ae-Lane") != s.connection.Branch || headers.Get("X-Kunlun-Token") == "" {
		return errors.New("浏览器授权与当前分支不一致，请重新授权")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	cookies := make([]*http.Cookie, 0, len(auth.Cookies))
	for _, entry := range auth.Cookies {
		cookie := &http.Cookie{Name: entry.Name, Value: entry.Value, Path: entry.Path, Secure: entry.Secure}
		if strings.HasPrefix(entry.Domain, ".") {
			cookie.Domain = entry.Domain
		} else if entry.Domain != dev.Hostname() {
			continue
		}
		if entry.Expires > 0 {
			cookie.Expires = time.Unix(int64(entry.Expires), 0)
		}
		cookies = append(cookies, cookie)
	}
	jar.SetCookies(dev, cookies)

	s.client = &http.Client{
		Transport: s.service.developerTransport,
		Jar:       jar,
		Timeout:   20 * time.Second,
		// 只访问明确的开发后台接口，任何重定向均由业务层判定。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.origin, s.headers = auth.Origin, headers
	s.generation++
	return nil
}

func (s *developerSession) request(ctx context.Context, method, path string, body []byte) (json.RawMessage, []byte, error) {
	s.mu.Lock()
	if s.client == nil {
		if err := s.authenticate(ctx, false); err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
	}
	s.mu.Unlock()
	data, raw, err := s.do(ctx, method, path, body)
	if !errors.Is(err, errDeveloperLogin) {
		return data, raw, err
	}
	var login developerLoginVersion
	_ = errors.As(err, &login)
	s.mu.Lock()
	// 其它并发请求已刷新后直接复用新会话重试。
	if s.refreshed {
		retry := login.generation < s.generation
		s.mu.Unlock()
		if retry {
			return s.do(ctx, method, path, body)
		}
		return data, raw, err
	}
	s.refreshed = true
	if err := s.authenticate(ctx, true); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("%w：%v", errDeveloperLogin, err)
	}
	s.mu.Unlock()
	return s.do(ctx, method, path, body)
}

func (s *developerSession) do(ctx context.Context, method, path string, body []byte) (json.RawMessage, []byte, error) {
	s.mu.Lock()
	client, origin, headers, generation := s.client, s.origin, s.headers.Clone(), s.generation
	s.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("无法构造开发后台请求")
	}
	req.Header = headers
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("开发后台请求失败或超时")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return nil, nil, developerLoginVersion{generation: generation}
	case http.StatusForbidden:
		return nil, nil, errDeveloperDenied
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if target, err := response.Location(); err == nil &&
			(target.Hostname() == "accounts.feishu.cn" || (target.Host == req.URL.Host &&
				(strings.Contains(target.Path, "/login/") || strings.Contains(target.Path, "/authen/")))) {
			return nil, nil, developerLoginVersion{generation: generation}
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("开发后台请求失败（HTTP %d）", response.StatusCode)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("无法读取开发后台响应")
	}
	var envelope struct {
		StatusCode json.RawMessage `json:"status_code"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, errors.New("开发后台返回无效 JSON")
	}
	if code := string(envelope.StatusCode); code != "" && code != "0" && code != `"0"` {
		return nil, nil, errors.New("开发后台返回业务错误")
	}
	return envelope.Data, raw, nil
}

func (s *developerSession) catalog(ctx context.Context, kind string) ([]developerAsset, error) {
	ns := url.PathEscape(s.connection.Namespace)
	method, path, body := http.MethodGet, "/ae/api/v4/describe/namespaces/"+ns+"/automations", []byte(nil)
	switch kind {
	case "pages":
		method, path, body = http.MethodPost, "/ae/api/v4/builder/namespaces/"+ns+"/describe/pages/list", []byte("{}")
	case "objects":
		return s.objectCatalog(ctx, "/ae/api/v1/describe/namespaces/"+ns+"/objects")
	}
	data, _, err := s.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return decodeDeveloperCatalog(data)
}

func (s *developerSession) objectCatalog(ctx context.Context, path string) ([]developerAsset, error) {
	var items []developerAsset
	seen := map[string]bool{}
	total := -1
	for {
		target := path
		if len(items) > 0 {
			target += "?offset=" + strconv.Itoa(len(items))
		}
		data, _, err := s.request(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		page, err := parseDeveloperCatalog(data)
		if err != nil {
			return nil, err
		}
		if total == -1 {
			total = page.total
		}
		if total < 0 || page.total != total || len(items)+len(page.items) > total ||
			(len(page.items) == 0 && len(items) < total) {
			return nil, errors.New("对象目录未完整加载")
		}
		for _, item := range page.items {
			if seen[item.id] {
				return nil, errors.New("对象目录分页返回了重复物料")
			}
			seen[item.id] = true
			items = append(items, item)
		}
		if len(items) == total {
			if page.more {
				return nil, errors.New("对象目录数量与分页状态不一致")
			}
			return items, nil
		}
	}
}
