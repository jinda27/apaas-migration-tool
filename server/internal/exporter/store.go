package exporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Store struct {
	mu   sync.RWMutex
	file string
	data state
}

func NewStore(dir string) (*Store, error) {
	// 任务状态与浏览器会话目录仅允许当前用户访问。
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{file: filepath.Join(dir, "state.json"), data: state{
		Connections: map[string]Connection{}, Materials: map[string]Material{}, Jobs: map[string]Job{},
	}}
	raw, err := os.ReadFile(s.file)
	// 首次运行没有历史状态。
	if os.IsNotExist(err) {
		s.data.Catalog = map[string]materialCatalog{}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, err
	}
	// 兼容缺少新增字段的历史状态。
	if s.data.Connections == nil {
		s.data.Connections = map[string]Connection{}
	}
	if s.data.Materials == nil {
		s.data.Materials = map[string]Material{}
	}
	if s.data.Jobs == nil {
		s.data.Jobs = map[string]Job{}
	}
	// 重启时保留已完成项，并标记尚未完成的任务。
	for id, job := range s.data.Jobs {
		if job.Tables == nil {
			job.Tables = map[string]string{}
		}
		if job.SourceErrors == nil {
			job.SourceErrors = map[string]string{}
		}
		if job.Items == nil {
			job.Items = []Item{}
		}
		if job.Events == nil {
			job.Events = []Event{}
		}
		if active(job.Status) {
			job.Status = "interrupted"
			job.Events = append(job.Events, Event{Time: time.Now(), Message: "服务重启，已完成文件和写入进度保留，可继续任务"})
		}
		s.data.Jobs[id] = job
	}
	if s.data.Catalog == nil {
		s.data.restoreCatalog()
	}
	return s, s.persist()
}

func active(status string) bool {
	return status == "queued" || status == "collecting" || status == "writing"
}

func (s *Store) persist() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	// 先写临时文件再替换，避免退出时截断历史记录。
	if err := os.WriteFile(s.file+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(s.file+".tmp", s.file)
}

func (s *Store) Change(fn func(*state)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	var previous state
	if err := json.Unmarshal(raw, &previous); err != nil {
		return err
	}
	fn(&s.data)
	// 保存失败回滚内存，不能展示尚未持久化的成功进度。
	if err := s.persist(); err != nil {
		s.data = previous
		return err
	}
	return nil
}

func (s *Store) Interrupt(id, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.data.Jobs[id]
	job.Status, job.UpdatedAt = "interrupted", time.Now()
	job.Events = append(job.Events, Event{Time: time.Now(), Message: redact(message)})
	s.data.Jobs[id] = job
	// 磁盘故障时仍在当前页面显示停止原因，历史状态可在重启后继续。
	_ = s.persist()
}

func (s *Store) Connection(id string) (Connection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.data.Connections[id]
	return c, ok
}

func (s *Store) Job(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.data.Jobs[id]
	// 通过序列化复制切片与映射，避免后台写入与响应读取竞争。
	raw, _ := json.Marshal(j)
	var result Job
	_ = json.Unmarshal(raw, &result)
	return result, ok
}

func (s *Store) Material(id string) (Material, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.data.Materials[id]
	return m, ok
}

func (s *Store) List() ([]Connection, []Job) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	connections := make([]Connection, 0, len(s.data.Connections))
	jobs := make([]Job, 0, len(s.data.Jobs))
	// 查询结果脱离内部可变状态。
	for _, c := range s.data.Connections {
		connections = append(connections, c)
	}
	for _, j := range s.data.Jobs {
		raw, _ := json.Marshal(j)
		var copy Job
		_ = json.Unmarshal(raw, &copy)
		jobs = append(jobs, copy)
	}
	sort.Slice(connections, func(i, j int) bool { return connections[i].UpdatedAt.After(connections[j].UpdatedAt) })
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	return connections, jobs
}

func (s *Store) Materials(connection string) []Material {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []Material{}
	// 仅返回当前连接所属物料。
	for _, m := range s.data.Materials {
		if m.ConnectionID == connection {
			result = append(result, m)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func (s *Store) CurrentMaterials(connection string) []Material {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []Material{}
	for _, entries := range s.data.Catalog[connection] {
		for _, id := range entries {
			if material, ok := s.data.Materials[id]; ok {
				result = append(result, material)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func (data *state) catalogFor(connection string) materialCatalog {
	catalog := data.Catalog[connection]
	if catalog == nil {
		catalog = materialCatalog{}
		data.Catalog[connection] = catalog
	}
	return catalog
}

func (catalog materialCatalog) add(material Material) {
	if catalog[material.Kind] == nil {
		catalog[material.Kind] = map[string]string{}
	}
	catalog[material.Kind][material.APIName] = material.ID
}

func (data *state) publishExport(job Job) materialCatalog {
	if job.Status != "completed" && job.Status != "partial" {
		return nil
	}
	exported := materialCatalog{}
	for _, kind := range job.Kinds {
		if job.SourceErrors[kind] == "" {
			exported[kind] = map[string]string{}
		}
	}
	failed := map[string]bool{}
	for _, item := range job.Items {
		material, ok := data.Materials[item.MaterialID]
		if !ok || material.ConnectionID != job.ConnectionID || material.Kind != item.Kind ||
			item.Status != "uploaded" || job.SourceErrors[item.Kind] != "" {
			failed[item.Kind] = true
			continue
		}
		exported.add(material)
	}
	for kind := range failed {
		delete(exported, kind)
	}
	if len(exported) > 0 {
		current := data.catalogFor(job.ConnectionID)
		for kind, items := range exported {
			current[kind] = items
		}
	}
	return exported
}

func (data *state) restoreCatalog() {
	data.Catalog = map[string]materialCatalog{}
	jobs := make([]Job, 0, len(data.Jobs))
	for _, job := range data.Jobs {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].UpdatedAt.Equal(jobs[j].UpdatedAt) {
			return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
		}
		return jobs[i].UpdatedAt.Before(jobs[j].UpdatedAt)
	})
	type catalogKind struct{ connection, kind string }
	publishedAt := map[catalogKind]time.Time{}
	for _, job := range jobs {
		for kind := range data.publishExport(job) {
			publishedAt[catalogKind{job.ConnectionID, kind}] = job.UpdatedAt
		}
	}
	for _, material := range data.Materials {
		published, ok := publishedAt[catalogKind{material.ConnectionID, material.Kind}]
		if ok && (material.Source != "imported" || !material.CreatedAt.After(published)) {
			continue
		}
		catalog := data.catalogFor(material.ConnectionID)
		previous := data.Materials[catalog[material.Kind][material.APIName]]
		if previous.ID == "" || material.CreatedAt.After(previous.CreatedAt) {
			catalog.add(material)
		}
	}
}
