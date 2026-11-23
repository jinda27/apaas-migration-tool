package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Service struct {
	store              *Store
	root               string
	dataDir            string
	exports            string
	runner             Runner
	browser            BrowserClient
	apaas              *APaaS
	developerTransport http.RoundTripper
	mu                 sync.Mutex
	running            map[string]context.CancelFunc
	writer             sync.Mutex
	auth               map[string]string
	closing            context.Context
	close              context.CancelFunc
	workers            sync.WaitGroup
}

var progressLanes = []struct {
	key, name string
	kinds     []string
}{
	{key: "pages", name: "页面", kinds: []string{"pages"}},
	{key: "flows", name: "流程", kinds: []string{"flows"}},
	{key: "source", name: "源码", kinds: []string{"functions", "components"}},
	{key: "objects", name: "对象元数据", kinds: []string{"objects"}},
}

func newProgress(kinds []string) []Progress {
	selected := map[string]bool{}
	// 选中的物料映射到固定四条进度线。
	for _, kind := range kinds {
		selected[kind] = true
	}
	result := make([]Progress, 0, len(progressLanes))
	for _, lane := range progressLanes {
		status := "skipped"
		// 任一物料命中时启用对应进度线。
		for _, kind := range lane.kinds {
			if selected[kind] {
				status = "waiting"
				break
			}
		}
		result = append(result, Progress{Key: lane.key, Name: lane.name, Status: status})
	}
	return result
}

func New(root, dataDir, exports string) (*Service, error) {
	store, err := NewStore(dataDir)
	if err != nil {
		return nil, err
	}
	// 导出文件独立于源代码，避免重试时覆盖工作目录。
	if err := os.MkdirAll(exports, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{store: store, root: root, dataDir: dataDir, exports: exports, runner: CommandRunner{},
		browser: &NodeBrowser{root: root, data: dataDir}, apaas: NewAPaaS(), running: map[string]context.CancelFunc{},
		auth: map[string]string{}, closing: ctx, close: cancel}, nil
}

func (s *Service) Close() {
	s.mu.Lock()
	s.close()
	// 关闭服务时取消所有导出和授权命令。
	for _, cancel := range s.running {
		cancel()
	}
	s.mu.Unlock()
	s.workers.Wait()
	s.browser.Close()
}

func (s *Service) Save(input ConnectionInput) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, jobs := s.store.List()
	for _, job := range jobs {
		// 执行期间保持来源与目标配置稳定。
		if job.ConnectionID == input.ID && active(job.Status) {
			return Connection{}, errors.New("请等待当前应用任务结束后修改连接")
		}
	}
	c, err := Normalize(input)
	if err != nil {
		return c, err
	}
	secretProvided := input.ClientSecret != ""
	if c.ID == "" {
		// 新实例的 API 凭证必须成对填写。
		if (c.ClientID == "") != (input.ClientSecret == "") {
			return c, errors.New("Client ID 和 Client Secret 需要同时填写")
		}
		c.ID = newID()
	} else {
		old, ok := s.store.Connection(c.ID)
		if !ok {
			return c, errors.New("连接不存在")
		}
		sameSource := old.Tenant == c.Tenant && old.Namespace == c.Namespace && old.Branch == c.Branch
		// 原连接允许 Secret 留空；修改 Client ID 或切换来源时必须同时提交新 Secret。
		if !secretProvided && (!sameSource || c.ClientID != old.ClientID) {
			return c, errors.New("修改应用、环境或 Client ID 时需要同时填写 Client Secret")
		}
		if secretProvided && c.ClientID == "" {
			return c, errors.New("Client ID 和 Client Secret 需要同时填写")
		}
		// 切换应用或环境时建立新连接，保留旧任务的来源身份。
		if !sameSource {
			c.ID = newID()
		}
	}
	err = s.store.Change(func(data *state) { data.Connections[c.ID] = c })
	if err == nil {
		s.apaas.Set(c.ID, c.ClientID, input.ClientSecret)
	}
	return c, err
}

func (s *Service) ConnectionView(c Connection) ConnectionView {
	clientID, configured := s.apaas.Status(c.ID)
	// 重启后 Secret 状态会失效，Client ID 仍从连接配置完整回显。
	if clientID != "" {
		c.ClientID = clientID
	}
	return ConnectionView{Connection: c, ClientSecretConfigured: configured}
}

func (s *Service) Snapshot() map[string]any {
	connections, jobs := s.store.List()
	views := make([]ConnectionView, 0, len(connections))
	for _, c := range connections {
		views = append(views, s.ConnectionView(c))
	}
	return map[string]any{"connections": views, "jobs": jobs, "kinds": Labels}
}

func (s *Service) Materials(id string) []Material { return s.store.CurrentMaterials(id) }
func (s *Service) Job(id string) (Job, bool)      { return s.store.Job(id) }

func (s *Service) Probe(ctx context.Context, id string) (map[string]Check, error) {
	c, ok := s.store.Connection(id)
	if !ok {
		return nil, errors.New("连接不存在")
	}
	result := map[string]Check{}
	var mu sync.Mutex
	var group sync.WaitGroup
	run := func(key string, fn func() Check) {
		group.Add(1)
		go func() {
			defer group.Done()
			check := fn()
			mu.Lock()
			result[key] = check
			mu.Unlock()
		}()
	}
	// 互不依赖的认证检查并发执行。
	run("api", func() Check {
		_, err := s.apaas.Token(ctx, id)
		if err != nil {
			return Check{Message: redact(err.Error())}
		}
		return Check{Ready: true, Message: "应用 API 凭证有效"}
	})
	run("ae", func() Check {
		raw, err := s.runner.Run(ctx, s.root, "ae", []string{"auth", "list"}, nil)
		if err != nil {
			return Check{Message: redact(err.Error())}
		}
		if !strings.Contains(string(raw), c.Tenant) {
			return Check{Message: "尚未授权目标租户 " + c.Tenant}
		}
		return Check{Ready: true, Message: "目标租户已有 ae 授权"}
	})
	run("lark", func() Check {
		raw, err := s.runner.Run(ctx, s.root, "lark-cli", []string{"auth", "status", "--json", "--verify"}, nil)
		if err != nil {
			return Check{Message: redact(err.Error())}
		}
		var envelope struct {
			Identities struct {
				User struct {
					Verified bool   `json:"verified"`
					UserName string `json:"userName"`
					Message  string `json:"message"`
				} `json:"user"`
			} `json:"identities"`
		}
		if err := parseJSON(raw, &envelope); err != nil {
			return Check{Message: err.Error()}
		}
		u := envelope.Identities.User
		return Check{Ready: u.Verified, Name: u.UserName, Message: redact(u.Message)}
	})
	run("browser", func() Check {
		raw, err := s.browser.Call(ctx, "check", c, nil)
		if err != nil {
			return Check{Message: err.Error()}
		}
		var result struct {
			Check
			AppName string `json:"appName"`
			DevURL  string `json:"devUrl"`
		}
		_ = json.Unmarshal(raw, &result)
		// 授权后读取应用名称和开发域名，补全连接配置。
		if result.Ready {
			_ = s.store.Change(func(data *state) {
				saved := data.Connections[id]
				if result.AppName != "" {
					saved.AppName = result.AppName
				}
				if result.DevURL != "" {
					dev, err := trustedURL(result.DevURL)
					if err == nil && strings.HasPrefix(dev.Hostname(), c.Tenant+"-") {
						saved.DevURL = dev.Scheme + "://" + dev.Host
					}
				}
				data.Connections[id] = saved
			})
		}
		return result.Check
	})
	group.Wait()
	return result, nil
}

func (s *Service) Authorize(ctx context.Context, id, kind string) (map[string]any, error) {
	c, ok := s.store.Connection(id)
	if !ok {
		return nil, errors.New("连接不存在")
	}
	switch kind {
	case "browser":
		raw, err := s.browser.Call(ctx, "authorize", c, nil)
		var result map[string]any
		_ = json.Unmarshal(raw, &result)
		return result, err
	case "ae":
		// CLI 登录由用户在实际浏览器中确认，接口立即返回。
		s.mu.Lock()
		key := "auth:" + id
		if s.closing.Err() != nil {
			s.mu.Unlock()
			return nil, errors.New("服务正在关闭")
		}
		if _, exists := s.running[key]; exists {
			s.mu.Unlock()
			return nil, errors.New("ae 授权正在进行，请完成浏览器操作")
		}
		authCtx, cancel := context.WithTimeout(s.closing, 5*time.Minute)
		s.running[key] = cancel
		s.workers.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.workers.Done()
			defer cancel()
			_, _ = s.runner.Run(authCtx, s.root, "ae", []string{"auth", "login", "--tenant", c.Tenant}, nil)
			s.mu.Lock()
			delete(s.running, key)
			s.mu.Unlock()
		}()
		return map[string]any{"message": "已发起 ae 登录，请在弹出的浏览器完成授权，再检查连接"}, nil
	case "lark":
		raw, err := s.runner.Run(ctx, s.root, "lark-cli", []string{"auth", "login", "--domain", "docs", "--domain", "drive", "--no-wait", "--json"}, nil)
		if err != nil {
			return nil, err
		}
		var result map[string]any
		if err := parseJSON(raw, &result); err != nil {
			return nil, err
		}
		link := stringValue(result, "verification_url")
		code := stringValue(result, "device_code")
		// CLI 不同输出版本可能把认证数据包在 data 内。
		if data, ok := result["data"].(map[string]any); ok {
			link, code = stringValue(data, "verification_url"), stringValue(data, "device_code")
		}
		if link == "" || code == "" {
			return nil, errors.New("lark-cli 未返回授权链接，请在终端执行 lark-cli auth login --domain docs --domain drive")
		}
		_, err = s.runner.Run(ctx, s.dataDir, "lark-cli", []string{"auth", "qrcode", link, "--output", "lark-" + id + ".png"}, nil)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.auth[id] = code
		s.mu.Unlock()
		return map[string]any{"url": link, "qrcode": "/api/connections/" + id + "/qrcode", "message": "授权后点击“完成飞书授权”"}, nil
	case "lark-complete":
		s.mu.Lock()
		code := s.auth[id]
		s.mu.Unlock()
		if code == "" {
			return nil, errors.New("请先发起飞书授权")
		}
		_, err := s.runner.Run(ctx, s.root, "lark-cli", []string{"auth", "login", "--device-code", code}, nil)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		delete(s.auth, id)
		s.mu.Unlock()
		return map[string]any{"message": "飞书用户身份已授权"}, nil
	default:
		return nil, errors.New("不支持的授权方式")
	}
}

func stringValue(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

func (s *Service) Start(input JobInput) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Err() != nil {
		return Job{}, errors.New("服务正在关闭")
	}
	c, ok := s.store.Connection(input.ConnectionID)
	if !ok {
		return Job{}, errors.New("请先保存应用连接")
	}
	if input.Mode != "online" && input.Mode != "imported" {
		return Job{}, errors.New("采集方式无效")
	}
	kinds := []string{}
	seen := map[string]bool{}
	for _, kind := range input.Kinds {
		// 校验并去重用户选中的物料类型。
		if !validKind(kind) {
			return Job{}, errors.New("物料类型无效")
		}
		if !seen[kind] {
			kinds = append(kinds, kind)
			seen[kind] = true
		}
	}
	if len(kinds) == 0 {
		return Job{}, errors.New("请至少选择一类物料")
	}
	// 同一连接只允许一个任务，避免重复创建相同目标记录。
	_, jobs := s.store.List()
	for _, existing := range jobs {
		if existing.ConnectionID == c.ID && active(existing.Status) {
			return Job{}, errors.New("当前应用已有任务正在执行")
		}
	}
	name := c.AppName
	if name == "" {
		name = c.Namespace
	}
	job := Job{ID: newID(), ConnectionID: c.ID, AppName: name, Kinds: kinds, Mode: input.Mode, Status: "queued",
		BaseURL: c.BaseURL, Tables: map[string]string{}, SourceErrors: map[string]string{}, Items: []Item{}, Events: []Event{},
		Progress: newProgress(kinds), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.store.Change(func(data *state) { data.Jobs[job.ID] = job }); err != nil {
		return Job{}, err
	}
	s.launchLocked(job.ID)
	return job, nil
}

func (s *Service) Retry(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Err() != nil {
		return errors.New("服务正在关闭")
	}
	job, ok := s.store.Job(id)
	if !ok {
		return errors.New("任务不存在")
	}
	_, running := s.running[id]
	if running || active(job.Status) {
		return errors.New("任务正在执行")
	}
	_, jobs := s.store.List()
	for _, existing := range jobs {
		if existing.ConnectionID == job.ConnectionID && active(existing.Status) {
			return errors.New("当前应用已有任务正在执行")
		}
	}
	// 保留成功项、record_id 和附件 token，继续未完成的步骤。
	if err := s.store.Change(func(data *state) {
		j := data.Jobs[id]
		j.Status = "queued"
		j.Progress = newProgress(j.Kinds)
		j.UpdatedAt = time.Now()
		data.Jobs[id] = j
	}); err != nil {
		return err
	}
	s.launchLocked(id)
	return nil
}

func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cancel, ok := s.running[id]
	if !ok {
		return errors.New("任务未在执行")
	}
	cancel()
	return nil
}

func (s *Service) launchLocked(id string) {
	ctx, cancel := context.WithCancel(s.closing)
	s.running[id] = cancel
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		defer func() { s.mu.Lock(); delete(s.running, id); s.mu.Unlock() }()
		if err := s.execute(ctx, id); err != nil {
			slog.Error("export stopped", "job", id, "error", redact(err.Error()))
			s.store.Interrupt(id, "任务已停止："+err.Error())
		}
	}()
}

func (s *Service) changeJob(id string, fn func(*Job)) error {
	return s.store.Change(func(data *state) {
		job := data.Jobs[id]
		fn(&job)
		job.UpdatedAt = time.Now()
		data.Jobs[id] = job
	})
}

func (s *Service) changeProgress(id, key string, fn func(*Progress)) error {
	return s.changeJob(id, func(job *Job) {
		// 历史任务首次继续时补齐四条进度线。
		if len(job.Progress) == 0 {
			job.Progress = newProgress(job.Kinds)
		}
		for index := range job.Progress {
			// 只更新目标并发线。
			if job.Progress[index].Key == key {
				fn(&job.Progress[index])
				return
			}
		}
	})
}

func (s *Service) event(id, message string) {
	if err := s.changeJob(id, func(job *Job) {
		job.Events = append(job.Events, Event{Time: time.Now(), Message: redact(message)})
		// 只保留最近的执行事件，避免状态文件无限增长。
		if len(job.Events) > 200 {
			job.Events = job.Events[len(job.Events)-200:]
		}
	}); err != nil {
		slog.Error("export event persistence failed", "job", id, "error", err)
	}
}

func (s *Service) File(id string) (Material, string, error) {
	m, ok := s.store.Material(id)
	if !ok {
		return m, "", errors.New("物料不存在")
	}
	path := filepath.Join(s.exports, m.Path)
	relative, err := filepath.Rel(s.exports, path)
	// 仅允许下载导出目录内的文件。
	if err != nil || strings.HasPrefix(relative, "..") {
		return m, "", errors.New("物料路径无效")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, "", err
	}
	// 下载和上传前核验实际字节，阻止本地文件被修改后写入错误哈希。
	if contentHash(raw) != m.SHA256 {
		return m, "", errors.New("原始文件校验失败，请重新采集或导入")
	}
	return m, path, nil
}

func (s *Service) QRCode(id string) (string, error) {
	if _, ok := s.store.Connection(id); !ok {
		return "", errors.New("连接不存在")
	}
	return filepath.Join(s.dataDir, "lark-"+id+".png"), nil
}

func (s *Service) Import(connection, kind, filename string, reader io.Reader) (Material, error) {
	c, ok := s.store.Connection(connection)
	if !ok || !validKind(kind) {
		return Material{}, errors.New("连接或物料类型无效")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, 32<<20+1))
	if err != nil || len(raw) > 32<<20 {
		return Material{}, errors.New("单个导入文件不能超过 32MB")
	}
	// 页面、流程和对象元数据必须是可解析的原始 JSON。
	if (kind == "pages" || kind == "flows" || kind == "objects") && !json.Valid(raw) {
		return Material{}, errors.New("请导入有效 JSON 文件")
	}
	name := strings.TrimSuffix(safeName(filename), filepath.Ext(filename))
	apiName := name
	var payload map[string]any
	projectPath, runtime := "", ""
	_ = json.Unmarshal(raw, &payload)
	// 编辑器对象定义的名称与 API 标识位于 object 节点。
	if kind == "objects" {
		if object, ok := payload["object"].(map[string]any); ok {
			payload = object
		}
	}
	if kind == "functions" || kind == "components" {
		// 源码包没有独立定义时保留原有 JSON 导入识别。
		if definition, sourcePath, sourceRuntime := importedProjectDefinition(raw, kind); definition != nil {
			payload, projectPath, runtime = definition, sourcePath, sourceRuntime
		}
	}
	if payload != nil {
		if value := stringValue(payload, "apiName"); value != "" {
			apiName = value
		} else if value := stringValue(payload, "api_name"); value != "" {
			apiName = value
		}
		if value := label(payload); value != "" {
			name = value
		} else if projectPath != "" {
			// 源码包未填写展示名称时与在线采集一致，使用 API 名称。
			name = apiName
		}
	}
	m, err := s.saveMaterial(c, kind, name, apiName, safeName(filename), raw, "imported")
	if err != nil {
		return m, err
	}
	if projectPath != "" {
		m.ProjectPath, m.Runtime = projectPath, runtime
	}
	err = s.store.Change(func(data *state) {
		m.CreatedAt = time.Now()
		data.Materials[m.ID] = m
		data.catalogFor(c.ID).add(m)
	})
	return m, err
}

func (s *Service) AddMessage(id string, err error) {
	s.event(id, fmt.Sprint(err))
}
