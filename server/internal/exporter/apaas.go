package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type credential struct {
	clientID string
	secret   string
	token    string
	expires  time.Time
}

type APaaS struct {
	mu          sync.Mutex
	credentials map[string]*credential
	client      *http.Client
	baseURL     string
}

func NewAPaaS() *APaaS {
	return &APaaS{credentials: map[string]*credential{}, baseURL: "https://ae-openapi.feishu.cn",
		client: &http.Client{Timeout: 45 * time.Second}}
}

func (a *APaaS) Set(id, clientID, secret string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// 空凭证不覆盖当前进程已保存的授权。
	if clientID != "" && secret != "" {
		a.credentials[id] = &credential{clientID: clientID, secret: secret}
	}
}

func (a *APaaS) Status(id string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.credentials[id]
	// 只返回可公开的 Client ID 和配置状态。
	if c == nil {
		return "", false
	}
	return c.clientID, c.secret != ""
}

func (a *APaaS) Token(ctx context.Context, id string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.credentials[id]
	// 凭证仅驻留内存，重启后需重新输入。
	if c == nil {
		return "", errors.New("请填写所选环境的 Client ID 和 Client Secret")
	}
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	result, err := a.request(ctx, http.MethodPost, "/auth/v1/appToken", "", map[string]string{"clientId": c.clientID, "clientSecret": c.secret})
	if err != nil {
		return "", err
	}
	c.token, _ = result["accessToken"].(string)
	if c.token == "" {
		return "", errors.New("应用 Token 响应为空")
	}
	c.expires = time.Now().Add(110 * time.Minute)
	return c.token, nil
}

func (a *APaaS) request(ctx context.Context, method, path, token string, body any) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	// 请求体为空的 GET 不发送 JSON null。
	if body == nil {
		raw = nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		response, err := a.client.Do(req)
		if err != nil {
			return nil, errors.New("aPaaS 网络请求失败，请检查网络连接")
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 20<<20))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		var envelope struct {
			Code any            `json:"code"`
			Msg  string         `json:"msg"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return nil, fmt.Errorf("aPaaS 返回非 JSON 响应（HTTP %d）", response.StatusCode)
		}
		if response.StatusCode < 300 && fmt.Sprint(envelope.Code) == "0" {
			return envelope.Data, nil
		}
		// 仅对服务端明确限流重试。
		if (response.StatusCode == 429 || fmt.Sprint(envelope.Code) == "k_op_ec_20003") && attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
				continue
			}
		}
		return nil, fmt.Errorf("aPaaS %v：%s", envelope.Code, redact(envelope.Msg))
	}
	return nil, errors.New("aPaaS 重试次数已用尽")
}

func (a *APaaS) Object(ctx context.Context, c Connection, apiName string) (map[string]any, error) {
	token, err := a.Token(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	return a.request(ctx, http.MethodGet, "/api/data/v1/namespaces/"+url.PathEscape(c.Namespace)+"/meta/objects/"+url.PathEscape(apiName), token, nil)
}

func (a *APaaS) Flow(ctx context.Context, c Connection, apiName string) ([]byte, error) {
	token, err := a.Token(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	result, err := a.request(ctx, http.MethodGet, "/api/flow/v1/namespaces/"+url.PathEscape(c.Namespace)+"/flow/"+url.PathEscape(apiName), token, nil)
	if err != nil {
		return nil, err
	}
	workflow, ok := result["workflow"].(string)
	// 保留完整定义，不把流程摘要当作流程 JSON。
	if !ok || !json.Valid([]byte(workflow)) {
		return nil, errors.New("workflow 不是有效 JSON，请使用编辑器导出或导入原始文件")
	}
	return []byte(workflow), nil
}
