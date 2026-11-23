package exporter

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type collectionProgress func(totalDelta, currentDelta int) error

func reportCollectionProgress(reporters []collectionProgress, totalDelta, currentDelta int) error {
	// 未接入任务进度时保持采集函数可独立复用。
	if len(reporters) == 0 || reporters[0] == nil {
		return nil
	}
	return reporters[0](totalDelta, currentDelta)
}

func label(data map[string]any) string {
	// 兼容编辑器导出与 Open API 的名称格式。
	for _, key := range []string{"label", "name", "title"} {
		switch value := data[key].(type) {
		case string:
			return value
		case []any:
			// 开发后台使用语言代码与文本数组保存名称。
			fallback := ""
			for _, item := range value {
				text, _ := item.(map[string]any)
				name := stringValue(text, "text")
				if name != "" && fmt.Sprint(text["language_code"]) == "2052" {
					return name
				}
				if fallback == "" {
					fallback = name
				}
			}
			if fallback != "" {
				return fallback
			}
		case map[string]any:
			for _, language := range []string{"zh_CN", "zh_cn", "en_US", "en_us"} {
				if name, ok := value[language].(string); ok && name != "" {
					return name
				}
			}
		}
	}
	return ""
}

func contentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Service) saveMaterial(c Connection, kind, name, apiName, filename string, raw []byte, source string) (Material, error) {
	hash := contentHash(raw)
	// 相同来源与内容复用已有文件，避免重试产生重复物料。
	for _, m := range s.store.Materials(c.ID) {
		if m.Kind == kind && m.APIName == apiName && m.SHA256 == hash && m.Source == source {
			// 缓存文件缺失或损坏时重新落盘。
			if _, _, err := s.File(m.ID); err == nil {
				return m, nil
			}
		}
	}
	m := Material{ID: newID(), ConnectionID: c.ID, Kind: kind, Name: name, APIName: apiName,
		Size: int64(len(raw)), SHA256: hash, Source: source, CreatedAt: time.Now()}
	m.Path = filepath.Join(c.ID, kind, m.ID, safeName(filename))
	path := filepath.Join(s.exports, m.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return m, err
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return m, err
	}
	var object map[string]any
	// 对象摘要从实际响应提取，原始字节仍完整保留。
	if kind == "objects" && json.Unmarshal(raw, &object) == nil {
		m.ObjectType = stringValue(object, "type")
		if nested, ok := object["object"].(map[string]any); ok {
			m.ObjectType = stringValue(nested, "type")
		}
		if fields, ok := object["fields"].([]any); ok {
			m.FieldCount = len(fields)
		}
	}
	err := s.store.Change(func(data *state) { data.Materials[m.ID] = m })
	return m, err
}

func (s *Service) collectAPI(ctx context.Context, session *developerSession, kind string, reporters ...collectionProgress) ([]Material, error) {
	c := session.connection
	// 在枚举之前验证 API 凭证，避免无效凭证触发浏览器操作。
	if _, err := s.apaas.Token(ctx, c.ID); err != nil {
		return nil, err
	}
	ids := c.FlowIDs
	if kind == "objects" {
		ids = c.ObjectIDs
		if len(ids) == 0 {
			objects, err := session.catalog(ctx, kind)
			if err != nil {
				return nil, err
			}
			for _, object := range objects {
				// 公开元数据接口使用 API 名称，内部标识仅用于开发后台。
				ids = append(ids, object.apiName)
			}
		}
	}
	materials, failures := []Material{}, []string{}
	seen := map[string]bool{}
	validIDs := make([]string, 0, len(ids))
	for _, apiName := range ids {
		if !identifier.MatchString(apiName) {
			return nil, errors.New("目录包含无效 API 名称")
		}
		if seen[apiName] {
			continue
		}
		seen[apiName] = true
		validIDs = append(validIDs, apiName)
	}
	if err := reportCollectionProgress(reporters, len(validIDs), 0); err != nil {
		return materials, err
	}
	for _, apiName := range validIDs {
		if err := ctx.Err(); err != nil {
			return materials, err
		}
		var raw []byte
		var err error
		name := apiName
		if kind == "objects" {
			var object map[string]any
			object, err = s.apaas.Object(ctx, c, apiName)
			if err == nil {
				raw, err = json.MarshalIndent(object, "", "  ")
				if value := label(object); value != "" {
					name = value
				}
			}
		} else {
			raw, err = s.apaas.Flow(ctx, c, apiName)
		}
		if err != nil {
			failures = append(failures, apiName+"："+err.Error())
			if progressErr := reportCollectionProgress(reporters, 0, 1); progressErr != nil {
				return materials, progressErr
			}
			continue
		}
		m, err := s.saveMaterial(c, kind, name, apiName, safeName(apiName)+".json", raw, "api")
		if err != nil {
			return materials, err
		}
		materials = append(materials, m)
		if err := reportCollectionProgress(reporters, 0, 1); err != nil {
			return materials, err
		}
		// 控制逐对象请求频率，低于官方凭证 QPS 限制。
		select {
		case <-ctx.Done():
			return materials, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(failures) > 0 {
		return materials, errors.New(strings.Join(failures, "；"))
	}
	return materials, nil
}

func ignoredFile(path string) bool {
	// 不归档依赖缓存、版本控制目录和常见凭证文件。
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == "node_modules" || segment == ".git" || segment == ".ae" || segment == ".env" ||
			strings.HasPrefix(segment, ".env.") || segment == ".DS_Store" || strings.Contains(strings.ToLower(segment), "credential") {
			return true
		}
	}
	return false
}

func projectScope(relative string) (string, string) {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	// 运行时目录是共享范围，物料边界由 index.meta.json 确定。
	for i, part := range parts {
		kind := ""
		if part == "functions" || part == "function" {
			kind = "functions"
		} else if part == "components" || part == "component" {
			kind = "components"
		}
		if kind != "" && i+1 < len(parts) {
			end := i + 1
			if kind == "functions" && (parts[i+1] == "nodejs" || parts[i+1] == "nodejs_20" || parts[i+1] == "golang") {
				end++
			}
			return kind, strings.Join(parts[:end], "/")
		}
	}
	return "", ""
}

func importedProjectDefinition(raw []byte, kind string) (map[string]any, string, string) {
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, "", ""
	}
	var meta map[string]any
	projectPath, runtime := "", ""
	// 单物料源码包从真实定义恢复工程路径和运行环境。
	for _, file := range archive.File {
		class, scope := projectScope(file.Name)
		if class != kind || filepath.Base(file.Name) != "index.meta.json" || ignoredFile(file.Name) {
			continue
		}
		// 多物料工程不猜测其唯一身份，保留文件导入语义。
		if meta != nil || file.UncompressedSize64 > 1<<20 {
			return nil, "", ""
		}
		reader, err := file.Open()
		if err != nil {
			return nil, "", ""
		}
		content, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
		_ = reader.Close()
		if err != nil || len(content) > 1<<20 || json.Unmarshal(content, &meta) != nil || meta == nil {
			return nil, "", ""
		}
		projectPath = filepath.ToSlash(filepath.Dir(file.Name))
		if kind == "functions" {
			runtime = filepath.Base(scope)
		}
	}
	return meta, projectPath, runtime
}

func (s *Service) cloneProject(ctx context.Context, c Connection, jobID string) (string, error) {
	directory := filepath.Join(s.exports, jobID, "project")
	location := filepath.Join(s.exports, jobID, "project-location.txt")
	if raw, err := os.ReadFile(location); err == nil {
		candidate := string(raw)
		relative, err := filepath.Rel(filepath.Join(s.exports, jobID), candidate)
		if err == nil && !strings.HasPrefix(relative, "..") {
			directory = candidate
		}
	}
	// 下载完成标记存在时复用工程，不重复运行 CLI。
	if _, err := os.Stat(filepath.Join(directory, ".download-complete")); err == nil {
		return directory, nil
	}
	// 失败目录保留以便检查，下一次使用新的目录克隆。
	if _, err := os.Stat(directory); err == nil {
		directory += "-" + newID()
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err := s.runner.Run(callCtx, s.root, "ae", []string{"project", "clone", c.Namespace, directory, "--tenant", c.Tenant, "--branch", c.Branch}, nil)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(directory, ".download-complete"), nil, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(location, []byte(directory), 0600); err != nil {
		return "", err
	}
	return directory, nil
}

func (s *Service) collectProject(c Connection, kind, directory string, reporters ...collectionProgress) ([]Material, error) {
	groups := map[string]map[string]any{}
	allGroups := map[string]bool{}
	files := []string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, _ := filepath.Rel(directory, path)
		if ignoredFile(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// 不读取符号链接，避免工程引用本机目录外的数据。
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || entry.Name() == ".download-complete" {
			return nil
		}
		files = append(files, relative)
		// 只将带实际物料定义的目录视作函数或组件。
		class, _ := projectScope(relative)
		if entry.Name() == "index.meta.json" && class != "" {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var meta map[string]any
			if err := json.Unmarshal(raw, &meta); err != nil {
				return fmt.Errorf("物料定义无效：%s", relative)
			}
			group := filepath.ToSlash(filepath.Dir(relative))
			allGroups[group] = true
			if class == kind {
				groups[group] = meta
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("工程中未找到 %s 目录，请导入对应源码包或核验实际工程结构", kind)
	}
	materials := []Material{}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sort.Strings(files)
	if err := reportCollectionProgress(reporters, len(keys), 0); err != nil {
		return materials, err
	}
	for _, group := range keys {
		var content bytes.Buffer
		archive := zip.NewWriter(&content)
		_, scope := projectScope(group + "/index.meta.json")
		// 每份源码包包含自身文件与公共配置、依赖声明。
		for _, relative := range files {
			class, fileScope := projectScope(relative)
			owner := ""
			// 嵌套目录按最长物料路径确定归属，公共代码与同运行时共享。
			for candidate := range allGroups {
				if strings.HasPrefix(filepath.ToSlash(relative), candidate+"/") && len(candidate) > len(owner) {
					owner = candidate
				}
			}
			if owner != group && !(owner == "" && class == kind && fileScope == scope) &&
				!(class == "" && filepath.Dir(relative) == ".") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(directory, relative))
			if err != nil {
				return materials, err
			}
			file, err := archive.Create(filepath.ToSlash(relative))
			if err != nil {
				return materials, err
			}
			if _, err := file.Write(raw); err != nil {
				return materials, err
			}
		}
		if err := archive.Close(); err != nil {
			return materials, err
		}
		meta := groups[group]
		apiName := stringValue(meta, "apiName")
		if apiName == "" {
			apiName = filepath.Base(group)
		}
		name := label(meta)
		if name == "" {
			name = apiName
		}
		m, err := s.saveMaterial(c, kind, name, apiName, safeName(apiName)+".zip", content.Bytes(), "ae")
		if err != nil {
			return materials, err
		}
		m.ProjectPath = group
		if kind == "functions" {
			m.Runtime = filepath.Base(scope)
		}
		if err := s.store.Change(func(data *state) { data.Materials[m.ID] = m }); err != nil {
			return materials, err
		}
		materials = append(materials, m)
		if err := reportCollectionProgress(reporters, 0, 1); err != nil {
			return materials, err
		}
	}
	return materials, nil
}

func selected(kinds []string, expected string) bool {
	// 任务物料数量很小，线性判断保持调用处直观。
	for _, kind := range kinds {
		if kind == expected {
			return true
		}
	}
	return false
}

func reusableItems(job Job, kind string) (int, bool) {
	count := 0
	// 已成功采集的类型在继续任务时直接计为完成。
	for _, item := range job.Items {
		if item.Kind == kind {
			count++
		}
	}
	return count, count > 0 && job.SourceErrors[kind] == ""
}

func projectMaterialCounts(directory string, kinds []string) (map[string]int, error) {
	wanted := map[string]bool{}
	// 仅统计本条源码线中本轮需要重新采集的类型。
	for _, kind := range kinds {
		wanted[kind] = true
	}
	counts := map[string]int{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, _ := filepath.Rel(directory, path)
		if ignoredFile(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// 每个有效 index.meta.json 对应一个源码物料。
		if !entry.IsDir() && entry.Name() == "index.meta.json" {
			kind, _ := projectScope(relative)
			if wanted[kind] {
				counts[kind]++
			}
		}
		return nil
	})
	return counts, err
}

func (s *Service) laneReporter(id, key string) collectionProgress {
	return func(totalDelta, currentDelta int) error {
		return s.changeProgress(id, key, func(progress *Progress) {
			progress.Total += totalDelta
			progress.Current += currentDelta
		})
	}
}

func (s *Service) cachedKindMaterials(connectionID, kind string) []Material {
	materials := []Material{}
	seen := map[string]bool{}
	for _, material := range s.store.CurrentMaterials(connectionID) {
		// 只复用当前目录中校验通过的同类物料。
		if material.Kind != kind || seen[material.APIName] {
			continue
		}
		if _, _, err := s.File(material.ID); err != nil {
			continue
		}
		seen[material.APIName] = true
		materials = append(materials, material)
	}
	return materials
}

func (s *Service) useCachedCollection(id string, c Connection, kind string, materials []Material,
	collectErr error, report collectionProgress) ([]Material, error) {
	if collectErr == nil {
		return materials, nil
	}
	cached := s.cachedKindMaterials(c.ID, kind)
	if len(cached) == 0 {
		return materials, collectErr
	}
	seen := map[string]bool{}
	for _, material := range materials {
		seen[material.APIName] = true
	}
	for _, material := range cached {
		// 实时结果优先，缓存只补齐缺失物料。
		if !seen[material.APIName] {
			materials = append(materials, material)
			seen[material.APIName] = true
		}
	}
	if len(materials) == len(cached) {
		job, _ := s.store.Job(id)
		expected := 0
		for _, progress := range job.Progress {
			if progress.Key == kind {
				expected = progress.Total
				break
			}
		}
		// 整类实时来源不可用时用缓存建立进度总数。
		if expected == 0 {
			if err := report(len(cached), len(cached)); err != nil {
				return materials, err
			}
		}
	}
	job, _ := s.store.Job(id)
	expected := 0
	for _, progress := range job.Progress {
		if progress.Key == kind {
			expected = progress.Total
			break
		}
	}
	// 缓存覆盖实时目录总数后，本轮可继续写入。
	if expected == 0 || len(materials) >= expected {
		s.event(id, Labels[kind]+"使用最近一次成功采集物料")
		return materials, nil
	}
	return materials, collectErr
}

func (s *Service) collectKind(ctx context.Context, id string, c Connection, kind, project string, projectErr error,
	session *developerSession, report collectionProgress) error {
	job, _ := s.store.Job(id)
	var materials []Material
	var collectErr error
	if job.Mode == "imported" {
		seen := map[string]bool{}
		// 导入模式按 API 名称去重后计算总数。
		for _, material := range s.store.Materials(c.ID) {
			if material.Kind == kind && material.Source == "imported" && !seen[material.APIName] {
				materials = append(materials, material)
				seen[material.APIName] = true
			}
		}
		if len(materials) == 0 {
			collectErr = errors.New("尚未导入 " + Labels[kind] + " 文件")
		} else if err := report(len(materials), len(materials)); err != nil {
			collectErr = err
		}
	} else {
		switch kind {
		case "pages":
			materials, collectErr = s.collectSessionAPI(ctx, session, kind, id, report)
		case "flows":
			// 明确提供 API 名称时优先走已公开的流程详情接口。
			if len(c.FlowIDs) > 0 {
				materials, collectErr = s.collectAPI(ctx, session, kind, report)
			} else {
				materials, collectErr = s.collectSessionAPI(ctx, session, kind, id, report)
			}
		case "objects":
			materials, collectErr = s.collectAPI(ctx, session, kind, report)
		case "functions", "components":
			if projectErr != nil {
				collectErr = projectErr
			} else {
				materials, collectErr = s.collectProject(c, kind, project, report)
			}
		}
	}
	if job.Mode == "online" && ctx.Err() == nil {
		materials, collectErr = s.useCachedCollection(id, c, kind, materials, collectErr, report)
	}

	current, _ := s.store.Job(id)
	materialKeys := map[string]string{}
	// 建立已有任务项的幂等键索引。
	for _, item := range current.Items {
		material, _ := s.store.Material(item.MaterialID)
		materialKeys[item.MaterialID] = uniqueKey(c, material)
	}
	if saveErr := s.changeJob(id, func(job *Job) {
		if collectErr != nil {
			job.SourceErrors[kind] = redact(collectErr.Error())
		} else {
			delete(job.SourceErrors, kind)
			collectedKeys := make(map[string]bool, len(materials))
			for _, material := range materials {
				collectedKeys[uniqueKey(c, material)] = true
			}
			// 成功重采时，以新目录为准，同时沿用仍存在的记录与附件。
			kept := job.Items[:0]
			for _, item := range job.Items {
				if item.Kind != kind || collectedKeys[materialKeys[item.MaterialID]] {
					kept = append(kept, item)
				}
			}
			job.Items = kept
		}
		existing := map[string]int{}
		for index, item := range job.Items {
			if item.Kind == kind {
				existing[materialKeys[item.MaterialID]] = index
			}
		}
		for _, material := range materials {
			key := uniqueKey(c, material)
			item := Item{MaterialID: material.ID, Name: material.Name, Kind: kind, Status: "collected"}
			if index, exists := existing[key]; exists {
				// 同一 API 新版本替换旧项并保留远端记录 ID。
				if job.Items[index].MaterialID != material.ID {
					item.RecordID = job.Items[index].RecordID
					job.Items[index] = item
				}
			} else {
				existing[key] = len(job.Items)
				job.Items = append(job.Items, item)
			}
		}
	}); saveErr != nil {
		return saveErr
	}
	if collectErr != nil {
		s.event(id, Labels[kind]+"采集失败："+collectErr.Error())
	} else {
		s.event(id, fmt.Sprintf("%s已收集 %d 项", Labels[kind], len(materials)))
	}
	return collectErr
}

func (s *Service) collectLane(ctx context.Context, id string, c Connection, job Job, laneKey string, laneKinds []string,
	session *developerSession) error {
	report := s.laneReporter(id, laneKey)
	selectedKinds := []string{}
	// 每条线只处理用户本次选中的物料类型。
	for _, kind := range laneKinds {
		if selected(job.Kinds, kind) {
			selectedKinds = append(selectedKinds, kind)
		}
	}
	if len(selectedKinds) == 0 {
		return nil
	}
	if err := s.changeProgress(id, laneKey, func(progress *Progress) { progress.Status = "running" }); err != nil {
		return err
	}

	needsCollection := []string{}
	successes := 0
	for _, kind := range selectedKinds {
		current, _ := s.store.Job(id)
		count, reusable := reusableItems(current, kind)
		if reusable {
			if err := report(count, count); err != nil {
				return err
			}
			successes++
			continue
		}
		needsCollection = append(needsCollection, kind)
	}

	project := ""
	var projectErr error
	if laneKey == "source" && job.Mode == "online" && len(needsCollection) > 0 {
		project, projectErr = s.cloneProject(ctx, c, id)
		if projectErr == nil {
			counts, err := projectMaterialCounts(project, needsCollection)
			if err != nil {
				projectErr = err
			} else {
				total := 0
				for _, kind := range needsCollection {
					total += counts[kind]
				}
				if err := report(total, 0); err != nil {
					return err
				}
			}
		}
	}

	failures := 0
	for _, kind := range needsCollection {
		kindReport := report
		// 源码总数已一次性汇总，子类型只递增完成数。
		if laneKey == "source" && job.Mode == "online" && projectErr == nil {
			kindReport = func(_ int, currentDelta int) error { return report(0, currentDelta) }
		}
		if err := s.collectKind(ctx, id, c, kind, project, projectErr, session, kindReport); err != nil {
			failures++
		} else {
			successes++
		}
	}

	status := "completed"
	if ctx.Err() != nil {
		status = "cancelled"
	} else if failures > 0 && successes > 0 {
		status = "partial"
	} else if failures > 0 {
		status = "failed"
	}
	return s.changeProgress(id, laneKey, func(progress *Progress) { progress.Status = status })
}

func (s *Service) execute(ctx context.Context, id string) error {
	// 失败或取消也生成清单，保留可下载的本地成果。
	defer func() {
		if err := s.makeArchive(id); err != nil {
			s.event(id, "本地归档失败："+err.Error())
		}
	}()
	job, _ := s.store.Job(id)
	c, _ := s.store.Connection(job.ConnectionID)
	if err := s.changeJob(id, func(j *Job) { j.Status = "collecting" }); err != nil {
		return err
	}
	s.event(id, "开始收集物料，已完成的文件与写入步骤会保留")
	var group sync.WaitGroup
	laneErrors := make(chan error, len(progressLanes))
	session := &developerSession{service: s, connection: c}
	for _, lane := range progressLanes {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := s.collectLane(ctx, id, c, job, lane.key, lane.kinds, session); err != nil {
				laneErrors <- err
			}
		}()
	}
	group.Wait()
	close(laneErrors)
	for err := range laneErrors {
		if err != nil && ctx.Err() == nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return s.changeJob(id, func(j *Job) { j.Status = "cancelled" })
	}
	job, _ = s.store.Job(id)
	// 已确认的空目录也能更新清单，无需创建空目标。
	if len(job.Items) == 0 {
		return s.finishExport(ctx, id, nil)
	}
	if err := s.changeJob(id, func(j *Job) { j.Status = "writing" }); err != nil {
		return err
	}
	s.event(id, "物料已保存到本地，等待多维表格写入")
	s.writer.Lock()
	var writeErr error
	if ctx.Err() == nil {
		writeErr = s.writeBase(ctx, id, c)
		if writeErr != nil {
			s.event(id, "多维表格写入失败："+writeErr.Error())
		}
	}
	s.writer.Unlock()
	return s.finishExport(ctx, id, writeErr)
}

func (s *Service) finishExport(ctx context.Context, id string, writeErr error) error {
	if err := s.store.Change(func(data *state) {
		job := data.Jobs[id]
		job.Status = "completed"
		success := 0
		nonempty := map[string]bool{}
		for _, item := range job.Items {
			nonempty[item.Kind] = true
			if item.Status == "uploaded" {
				success++
			} else {
				job.Status = "partial"
			}
		}
		for _, kind := range job.Kinds {
			if !nonempty[kind] && job.SourceErrors[kind] == "" {
				success++
			}
		}
		if len(job.SourceErrors) > 0 || writeErr != nil {
			job.Status = "partial"
		}
		if success == 0 {
			job.Status = "failed"
		}
		if ctx.Err() != nil {
			job.Status = "cancelled"
		}
		job.UpdatedAt = time.Now()
		data.Jobs[id] = job
		// 将显示清单和终态一并持久化，进度收起后立即可读本次结果。
		data.publishExport(job)
	}); err != nil {
		return err
	}
	s.event(id, "本轮结束，成功项已保留，未完成项可继续")
	return nil
}

func (s *Service) makeArchive(id string) error {
	job, _ := s.store.Job(id)
	dir := filepath.Join(s.exports, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0600); err != nil {
		return err
	}
	file, err := os.Create(filepath.Join(dir, "export.zip.tmp"))
	if err != nil {
		return err
	}
	defer file.Close()
	archive := zip.NewWriter(file)
	entry, err := archive.Create("manifest.json")
	if err != nil {
		return err
	}
	_, _ = entry.Write(manifest)
	for _, item := range job.Items {
		m, path, err := s.File(item.MaterialID)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entry, err := archive.Create(filepath.ToSlash(filepath.Join(Labels[m.Kind], m.ID, filepath.Base(path))))
		if err != nil {
			return err
		}
		if _, err := entry.Write(raw); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, "export.zip.tmp"), filepath.Join(dir, "export.zip"))
}

func (s *Service) Archive(id string) (string, error) {
	job, ok := s.store.Job(id)
	if !ok || active(job.Status) {
		return "", errors.New("任务不存在或仍在执行")
	}
	path := filepath.Join(s.exports, id, "export.zip")
	// 首次失败无文件、或归档尚未生成时按需创建清单。
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := s.makeArchive(id); err != nil {
			return "", err
		}
	}
	return path, nil
}
