package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type developerAsset struct {
	id, apiName, name    string
	terminal, workflowID string
	builderVersion       int
	metadata             map[string]any
}

type developerCatalog struct {
	items []developerAsset
	index map[string]int
	total int
	more  bool
}

func decodeDeveloperCatalog(raw json.RawMessage) ([]developerAsset, error) {
	catalog, err := parseDeveloperCatalog(raw)
	if err != nil {
		return nil, err
	}
	if catalog.more || catalog.total != len(catalog.items) {
		return nil, fmt.Errorf("目录未完整加载（已发现 %d / %s）", len(catalog.items), catalog.totalLabel())
	}
	return catalog.items, nil
}

func parseDeveloperCatalog(raw json.RawMessage) (developerCatalog, error) {
	var data any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return developerCatalog{}, errors.New("开发后台未返回有效目录")
	}
	catalog := developerCatalog{index: map[string]int{}, total: -1}
	catalog.inspect(data)
	return catalog, nil
}

func (c *developerCatalog) totalLabel() string {
	if c.total < 0 {
		return "未知"
	}
	return strconv.Itoa(c.total)
}

func (c *developerCatalog) inspect(value any) {
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			c.inspect(child)
		}
	case map[string]any:
		c.add(node)
		for _, key := range []string{"total", "totalCount"} {
			if total, err := strconv.Atoi(fmt.Sprint(node[key])); err == nil && total >= 0 {
				c.total = max(c.total, total)
				break
			}
		}
		c.more = c.more || node["has_more"] == true || node["hasMore"] == true
		// 目录容器之外的数据可能是节点和参数定义。
		for _, key := range []string{"data", "items", "list", "pages", "flows", "automations", "objects", "records", "results"} {
			c.inspect(node[key])
		}
	}
}

func (c *developerCatalog) add(data map[string]any) {
	var id string
	for _, key := range []string{"apiName", "api_name", "flowApiName", "page_api_name", "object_api_name"} {
		if data[key] != nil {
			id = stringValue(data, key)
			break
		}
	}
	name := label(data)
	if !identifier.MatchString(id) || name == "" || data["fieldType"] != nil {
		return
	}
	apiName := stringValue(data, "api_alias")
	if apiName == "" {
		apiName = id
	}
	version, _ := strconv.Atoi(fmt.Sprint(data["builder_version"]))
	item := developerAsset{id: id, apiName: apiName, name: name, metadata: data,
		terminal: stringValue(data, "terminal_type"), builderVersion: version,
		workflowID: stringValue(data, "source_workflow_id")}
	if index, ok := c.index[id]; ok {
		c.items[index] = item
		return
	}
	c.index[id] = len(c.items)
	c.items = append(c.items, item)
}

func (s *developerSession) definition(ctx context.Context, kind string, item developerAsset) ([]byte, error) {
	ns := url.PathEscape(s.connection.Namespace)
	if kind == "pages" {
		if item.builderVersion != 3 || (item.terminal != "pc" && item.terminal != "mobile" && item.terminal != "web") {
			return nil, errors.New("当前页面编辑器版本或终端类型尚未验证")
		}
		path := "/ae/api/v3/builder/namespaces/" + ns + "/describe/pageinfo/get?api_name=" + url.QueryEscape(item.id)
		data, raw, err := s.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			ID       string          `json:"api_name"`
			Specific json.RawMessage `json:"page_specific"`
		}
		if json.Unmarshal(data, &page) != nil || page.ID != item.id || len(page.Specific) == 0 || string(page.Specific) == "null" {
			return nil, errors.New("页面定义与目录标识不一致")
		}
		return raw, nil
	}

	if item.workflowID == "" || strings.Trim(item.workflowID, "0123456789") != "" {
		return nil, errors.New("流程目录缺少 source_workflow_id")
	}
	payload, err := json.Marshal(map[string]string{"source_workflow_id": item.workflowID})
	if err != nil {
		return nil, err
	}
	data, _, err := s.request(ctx, http.MethodPost, "/ae/api/v1/workflow/namespaces/"+ns+"/wf/export", payload)
	if err != nil {
		return nil, err
	}
	var flow struct {
		Workflow struct {
			ID json.Number `json:"sourceWorkflowID"`
		} `json:"workflow_json"`
	}
	if json.Unmarshal(data, &flow) != nil || flow.Workflow.ID.String() != item.workflowID {
		return nil, errors.New("流程导出定义与目录标识不一致")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

type developerCheckpoint struct {
	Name     string          `json:"name"`
	APIName  string          `json:"apiName"`
	Path     string          `json:"path"`
	Source   string          `json:"source"`
	Method   string          `json:"method"`
	SHA256   string          `json:"sha256"`
	Metadata json.RawMessage `json:"metadata"`
}

func (s *Service) collectSessionAPI(ctx context.Context, session *developerSession, kind, jobID string, reporters ...collectionProgress) ([]Material, error) {
	items, err := session.catalog(ctx, kind)
	if err != nil {
		return nil, err
	}
	if err := reportCollectionProgress(reporters, len(items), 0); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.exports, jobID, "downloads", kind)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	checkpointPath := filepath.Join(dir, "checkpoint.json")
	checkpoints := map[string]developerCheckpoint{}
	if raw, err := os.ReadFile(checkpointPath); err == nil {
		if json.Unmarshal(raw, &checkpoints) != nil || checkpoints == nil {
			checkpoints = map[string]developerCheckpoint{}
		}
	}
	materials, failures := []Material{}, []string{}
	failurePath := filepath.Join(dir, "failures.json")
	if err := writeCollectionJSON(failurePath, failures); err != nil {
		return nil, err
	}
	suffix, source := ".json", "api"
	if kind == "pages" {
		suffix, source = ".definition.json", "api-definition"
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return materials, err
		}
		filename := item.id + suffix
		path := filepath.Join(dir, filename)
		saved := checkpoints[item.id]
		var raw []byte
		// 兼容 Node 写入的断点，校验文件位置与哈希后再复用。
		if saved.Method == "api" && saved.Path == path && saved.APIName == item.apiName && saved.Source == source {
			if cached, err := os.ReadFile(path); err == nil && json.Valid(cached) && contentHash(cached) == saved.SHA256 {
				raw = cached
			}
		}
		if raw == nil {
			raw, err = session.definition(ctx, kind, item)
			if err != nil {
				if ctx.Err() != nil {
					return materials, ctx.Err()
				}
				failures = append(failures, item.name+"："+err.Error())
				if err := writeCollectionJSON(failurePath, failures); err != nil {
					return materials, err
				}
				if err := reportCollectionProgress(reporters, 0, 1); err != nil {
					return materials, err
				}
				if errors.Is(err, errDeveloperLogin) || errors.Is(err, errDeveloperDenied) {
					break
				}
				continue
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				return materials, err
			}
			metadata, err := json.Marshal(item.metadata)
			if err != nil {
				return materials, err
			}
			checkpoints[item.id] = developerCheckpoint{Name: item.name, APIName: item.apiName, Path: path, Source: source,
				Method: "api", SHA256: contentHash(raw), Metadata: metadata}
			if err := writeCollectionJSON(checkpointPath, checkpoints); err != nil {
				return materials, err
			}
		}
		material, err := s.saveMaterial(session.connection, kind, item.name, item.apiName, filename, raw, source)
		if err != nil {
			return materials, err
		}
		materials = append(materials, material)
		if err := reportCollectionProgress(reporters, 0, 1); err != nil {
			return materials, err
		}
	}
	if len(failures) > 0 {
		return materials, errors.New(strings.Join(failures, "；"))
	}
	return materials, nil
}

func writeCollectionJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
