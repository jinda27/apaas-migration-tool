package exporter

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var Kinds = []string{"pages", "flows", "functions", "components", "objects"}
var Labels = map[string]string{"pages": "页面", "flows": "流程", "functions": "云函数", "components": "自定义组件", "objects": "对象元数据"}
var identifier = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

type Connection struct {
	ID        string    `json:"id"`
	AppURL    string    `json:"appUrl"`
	DevURL    string    `json:"devUrl"`
	AppName   string    `json:"appName"`
	Tenant    string    `json:"tenant"`
	Namespace string    `json:"namespace"`
	Branch    string    `json:"branch"`
	BaseURL   string    `json:"baseUrl"`
	ObjectIDs []string  `json:"objectIds"`
	FlowIDs   []string  `json:"flowIds"`
	ClientID  string    `json:"clientId"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type ConnectionInput struct {
	Connection
	ClientSecret string `json:"clientSecret"`
}

type ConnectionView struct {
	Connection
	ClientSecretConfigured bool `json:"clientSecretConfigured"`
}

type Check struct {
	Ready   bool   `json:"ready"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

type Material struct {
	ID           string    `json:"id"`
	ConnectionID string    `json:"connectionId"`
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	APIName      string    `json:"apiName"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
	Source       string    `json:"source"`
	Runtime      string    `json:"runtime,omitempty"`
	ProjectPath  string    `json:"projectPath,omitempty"`
	ObjectType   string    `json:"objectType,omitempty"`
	FieldCount   int       `json:"fieldCount,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

type Item struct {
	MaterialID string `json:"materialId"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	RecordID   string `json:"recordId,omitempty"`
	FileToken  string `json:"fileToken,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Event struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

type Progress struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Current int    `json:"current"`
	Total   int    `json:"total"`
	Status  string `json:"status"`
}

type Job struct {
	ID           string            `json:"id"`
	ConnectionID string            `json:"connectionId"`
	AppName      string            `json:"appName"`
	Kinds        []string          `json:"kinds"`
	Mode         string            `json:"mode"`
	Status       string            `json:"status"`
	BaseURL      string            `json:"baseUrl"`
	BaseToken    string            `json:"baseToken"`
	Tables       map[string]string `json:"tables"`
	SourceErrors map[string]string `json:"sourceErrors"`
	Items        []Item            `json:"items"`
	Events       []Event           `json:"events"`
	Progress     []Progress        `json:"progress"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

type JobInput struct {
	ConnectionID string   `json:"connectionId"`
	Kinds        []string `json:"kinds"`
	Mode         string   `json:"mode"`
}

type state struct {
	Connections map[string]Connection      `json:"connections"`
	Materials   map[string]Material        `json:"materials"`
	Jobs        map[string]Job             `json:"jobs"`
	Catalog     map[string]materialCatalog `json:"catalog"`
}

// materialCatalog 按类型和 API 名称索引实例当前可见的物料。
type materialCatalog map[string]map[string]string

func newID() string {
	bytes := make([]byte, 12)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func validKind(kind string) bool {
	_, ok := Labels[kind]
	return ok
}

func Normalize(input ConnectionInput) (Connection, error) {
	c := input.Connection
	parsed, err := trustedURL(c.AppURL)
	// 只接受 aPaaS 官方租户入口。
	if err != nil || !strings.HasSuffix(parsed.Hostname(), ".feishu.cn") {
		return c, errors.New("请输入 https://租户.feishu.cn/ae/app_manage/应用命名空间/overview")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	// 从应用首页解析租户与命名空间。
	if len(parts) < 4 || parts[0] != "ae" || parts[1] != "app_manage" || !identifier.MatchString(parts[2]) {
		return c, errors.New("应用地址不包含有效的应用命名空间")
	}
	c.AppURL = parsed.String()
	c.Tenant = strings.Split(parsed.Hostname(), ".")[0]
	c.Namespace = parts[2]
	c.AppName = strings.TrimSpace(c.AppName)
	// 应用名称在授权后自动发现，也允许用户填写。
	if len([]rune(c.AppName)) > 100 {
		return c, errors.New("应用名称不能超过 100 字")
	}
	c.ClientID = strings.TrimSpace(c.ClientID)
	// Client ID 明文保存用于编辑回显，但仍限制为平台标识格式。
	if c.ClientID != "" && !identifier.MatchString(c.ClientID) {
		return c, errors.New("Client ID 格式无效")
	}
	c.Branch = strings.TrimSpace(c.Branch)
	// 未指定分支时使用开发中版本。
	if c.Branch == "" {
		c.Branch = "develop"
	}
	if !identifier.MatchString(c.Branch) {
		return c, errors.New("环境名称只能包含字母、数字、下划线、点和短横线")
	}
	// 开发地址限定在同一租户，防止凭证发往任意站点。
	if c.DevURL != "" {
		dev, err := trustedURL(c.DevURL)
		if err != nil || !strings.HasSuffix(dev.Hostname(), ".aedev.feishuapp.cn") || !strings.HasPrefix(dev.Hostname(), c.Tenant+"-") {
			return c, errors.New("开发地址必须属于源应用的 aedev.feishuapp.cn 租户")
		}
		c.DevURL = dev.Scheme + "://" + dev.Host
	}
	// 目标 Base 仅接受官方飞书链接。
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	if c.BaseURL != "" {
		base, err := trustedURL(c.BaseURL)
		if err != nil || !strings.Contains(base.Path, "/base/") {
			return c, errors.New("请输入有效的飞书多维表格 /base/ 链接")
		}
	}
	for _, id := range append(append([]string{}, c.ObjectIDs...), c.FlowIDs...) {
		// 手工补充的 API 名称也必须是合法标识。
		if !identifier.MatchString(id) {
			return c, errors.New("对象和流程 API 名称包含无效字符")
		}
	}
	c.UpdatedAt = time.Now()
	return c, nil
}

func trustedURL(value string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	// 不允许用户信息、非 HTTPS 或非官方域名。
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		(!strings.HasSuffix(u.Hostname(), ".feishu.cn") && !strings.HasSuffix(u.Hostname(), ".aedev.feishuapp.cn")) {
		return &url.URL{}, errors.New("无效的飞书地址")
	}
	u.Fragment = ""
	return u, nil
}

func uniqueKey(c Connection, m Material) string {
	return strings.Join([]string{c.Tenant, c.Namespace, c.Branch, m.Kind, m.APIName}, ":")
}

func safeName(value string) string {
	value = filepath.Base(strings.ReplaceAll(value, "\\", "/"))
	value = strings.Map(func(r rune) rune {
		// 去掉文件名中的控制字符和路径分隔符。
		if r < 32 || strings.ContainsRune(`/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, value)
	if value == "" || value == "." || value == ".." {
		return "material"
	}
	return value
}
