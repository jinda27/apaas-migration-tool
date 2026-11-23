package exporter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func fields(kind string) []map[string]any {
	result := []map[string]any{
		{"name": "名称", "type": "text"}, {"name": "unique_key", "type": "text"},
		{"name": "API名称", "type": "text"}, {"name": "环境", "type": "text"},
		{"name": "内容摘要", "type": "text"}, {"name": "原始文件", "type": "attachment"},
		{"name": "SHA256", "type": "text"}, {"name": "导出时间", "type": "datetime"},
	}
	if kind == "functions" || kind == "components" {
		result = append(result, map[string]any{"name": "工程路径", "type": "text"})
	}
	if kind == "functions" {
		result = append(result, map[string]any{"name": "运行环境", "type": "text"})
	}
	if kind == "objects" {
		result = append(result, map[string]any{"name": "对象类型", "type": "text"}, map[string]any{"name": "字段数", "type": "number"})
	}
	return result
}

func (s *Service) prepareBase(ctx context.Context, id string, c Connection) error {
	job, _ := s.store.Job(id)
	// 已记录的目标 Base 在继续任务时直接复用。
	if job.BaseToken == "" {
		token, link := "", job.BaseURL
		if link == "" {
			// 较早的任务可能在实例首次绑定 Base 前创建。
			link = c.BaseURL
		}
		create := link == ""
		if !create {
			u, err := trustedURL(link)
			if err != nil {
				return err
			}
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			for i, part := range parts {
				if part == "base" && i+1 < len(parts) {
					token = parts[i+1]
					break
				}
			}
			if _, err := s.lark(ctx, []string{"base", "+base-get", "--base-token", token}, nil); err != nil {
				return err
			}
		} else {
			schema, _ := json.Marshal(fields("pages"))
			data, err := s.lark(ctx, []string{"base", "+base-create", "--name", job.AppName + "物料数据", "--table-name", "页面", "--fields", string(schema)}, nil)
			if err != nil {
				return err
			}
			base, _ := data["base"].(map[string]any)
			token, link = stringValue(base, "base_token"), stringValue(base, "url")
			if token == "" {
				return errors.New("创建 Base 的响应缺少 base_token")
			}
		}
		// 同时保存任务目标和新建 Base 的实例绑定，恢复时可直接复用。
		if err := s.store.Change(func(data *state) {
			current := data.Jobs[id]
			current.BaseToken, current.BaseURL, current.UpdatedAt = token, link, time.Now()
			data.Jobs[id] = current
			if saved := data.Connections[c.ID]; create && saved.BaseURL == "" {
				saved.BaseURL = link
				data.Connections[c.ID] = saved
			}
		}); err != nil {
			return err
		}
	}
	job, _ = s.store.Job(id)
	data, err := s.lark(ctx, []string{"base", "+table-list", "--base-token", job.BaseToken}, nil)
	if err != nil {
		return err
	}
	tables := map[string]string{}
	if list, ok := data["tables"].([]any); ok {
		for _, raw := range list {
			table, _ := raw.(map[string]any)
			tables[stringValue(table, "name")] = stringValue(table, "id")
		}
	}
	// 五张表按顺序创建，避免平台建表限流。
	for _, kind := range Kinds {
		tableID := tables[Labels[kind]]
		if tableID == "" {
			schema, _ := json.Marshal(fields(kind))
			data, err := s.lark(ctx, []string{"base", "+table-create", "--base-token", job.BaseToken, "--name", Labels[kind], "--fields", string(schema)}, nil)
			if err != nil {
				return err
			}
			table, _ := data["table"].(map[string]any)
			tableID = stringValue(table, "id")
			if tableID == "" {
				return errors.New("建表响应缺少 table_id")
			}
		}
		data, err := s.lark(ctx, []string{"base", "+field-list", "--base-token", job.BaseToken, "--table-id", tableID}, nil)
		if err != nil {
			return err
		}
		available := map[string]string{}
		if list, ok := data["fields"].([]any); ok {
			for _, raw := range list {
				field, _ := raw.(map[string]any)
				available[stringValue(field, "name")] = stringValue(field, "type")
			}
		}
		missing := []map[string]any{}
		for _, field := range fields(kind) {
			name, _ := field["name"].(string)
			expected, _ := field["type"].(string)
			if actual, ok := available[name]; !ok {
				missing = append(missing, field)
			} else if actual != expected {
				return fmt.Errorf("%s表的“%s”字段类型为 %s，预期 %s", Labels[kind], name, actual, expected)
			}
		}
		if len(missing) > 0 {
			if _, err := s.lark(ctx, []string{"base", "+field-create", "--base-token", job.BaseToken, "--table-id", tableID}, missing); err != nil {
				return err
			}
		}
		if err := s.changeJob(id, func(j *Job) { j.Tables[kind] = tableID }); err != nil {
			return err
		}
	}
	return nil
}

type existingRecord struct {
	ID    string
	Hash  string
	Files []string
}

func (s *Service) recordIndex(ctx context.Context, job Job, kind string) (map[string]existingRecord, error) {
	index := map[string]existingRecord{}
	file := job.ID + "-" + kind + ".ndjson"
	offset := 0
	// 分页查重，只投影幂等键、内容哈希和附件。
	for {
		data, err := s.larkAt(ctx, s.dataDir, []string{"base", "+record-list", "--base-token", job.BaseToken, "--table-id", job.Tables[kind],
			"--field-id", "unique_key", "--field-id", "SHA256", "--field-id", "原始文件", "--offset", fmt.Sprint(offset),
			"--format", "ndjson", "--output", file, "--overwrite"}, nil)
		if err != nil {
			return nil, err
		}
		stream, err := os.Open(filepath.Join(s.dataDir, file))
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		count := 0
		for scanner.Scan() {
			var row map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
				_ = stream.Close()
				return nil, err
			}
			values := row
			if nested, ok := row["fields"].(map[string]any); ok {
				values = nested
			}
			key := stringValue(values, "unique_key")
			recordID := stringValue(row, "record_id")
			if recordID == "" {
				recordID = stringValue(row, "id")
			}
			if key != "" && recordID != "" {
				fileTokens := []string{}
				if attachments, ok := values["原始文件"].([]any); ok && len(attachments) > 0 {
					// 收集全部附件，重导时仅保留当前版本。
					for _, raw := range attachments {
						attachment, _ := raw.(map[string]any)
						fileToken := stringValue(attachment, "file_token")
						if fileToken == "" {
							fileToken = stringValue(attachment, "fileToken")
						}
						if fileToken != "" {
							fileTokens = append(fileTokens, fileToken)
						}
					}
				}
				index[key] = existingRecord{ID: recordID, Hash: stringValue(values, "SHA256"), Files: fileTokens}
			}
			count++
		}
		readErr := scanner.Err()
		_ = stream.Close()
		_ = os.Remove(filepath.Join(s.dataDir, file))
		if readErr != nil {
			return nil, readErr
		}
		if more, _ := data["has_more"].(bool); !more {
			break
		}
		if count == 0 {
			return nil, errors.New("分页响应未前进，已停止查重")
		}
		offset += count
	}
	return index, nil
}

func cells(c Connection, m Material) map[string]any {
	result := map[string]any{"名称": m.Name, "unique_key": uniqueKey(c, m), "API名称": m.APIName,
		"环境": c.Branch, "内容摘要": fmt.Sprintf("%s · %d 字节 · %s", Labels[m.Kind], m.Size, m.Source),
		"导出时间": m.CreatedAt.Format("2006-01-02 15:04")}
	if m.Kind == "functions" || m.Kind == "components" {
		result["工程路径"] = m.APIName
		if m.ProjectPath != "" {
			result["工程路径"] = m.ProjectPath
		}
	}
	if m.Kind == "functions" {
		result["运行环境"] = m.Runtime
	}
	if m.Kind == "objects" {
		result["对象类型"], result["字段数"] = m.ObjectType, m.FieldCount
	}
	return result
}

func (s *Service) setItem(id, material string, fn func(*Item)) error {
	return s.changeJob(id, func(job *Job) {
		for i := range job.Items {
			if job.Items[i].MaterialID == material {
				fn(&job.Items[i])
				break
			}
		}
	})
}

func (s *Service) writeBase(ctx context.Context, id string, c Connection) error {
	if err := s.prepareBase(ctx, id, c); err != nil {
		return err
	}
	var group sync.WaitGroup
	failures := make(chan error, len(Kinds))
	// 不同表独立并行，同一表的附件和记录更新保持串行。
	for _, kind := range Kinds {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := s.writeKind(ctx, id, c, kind); err != nil {
				failures <- fmt.Errorf("%s：%w", Labels[kind], err)
			}
		}()
	}
	group.Wait()
	close(failures)
	var result error
	for err := range failures {
		result = errors.Join(result, err)
	}
	return result
}

func (s *Service) writeKind(ctx context.Context, id string, c Connection, kind string) error {
	job, _ := s.store.Job(id)
	pending := []Item{}
	for _, item := range job.Items {
		if item.Kind == kind && item.Status != "uploaded" {
			pending = append(pending, item)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	index, err := s.recordIndex(ctx, job, kind)
	if err != nil {
		return err
	}
	creates, newItems := []map[string]any{}, []Item{}
	for _, item := range pending {
		m, _, err := s.File(item.MaterialID)
		if err != nil {
			return err
		}
		record, exists := index[uniqueKey(c, m)]
		if exists {
			if err := s.setItem(id, m.ID, func(i *Item) {
				i.RecordID, i.Status, i.Error = record.ID, "recorded", ""
				// 哈希匹配且仅有一个附件时跳过写入。
				if record.Hash == m.SHA256 && len(record.Files) == 1 {
					i.FileToken, i.Status = record.Files[0], "uploaded"
				} else if record.Hash == m.SHA256 && len(record.Files) > 1 {
					i.FileToken = record.Files[len(record.Files)-1]
				}
			}); err != nil {
				return err
			}
		} else if item.RecordID == "" {
			creates = append(creates, cells(c, m))
			newItems = append(newItems, item)
		}
	}
	// 新增记录按平台上限分批，同一张表串行写入。
	for start := 0; start < len(creates); start += 200 {
		end := min(start+200, len(creates))
		data, err := s.lark(ctx, []string{"base", "+record-batch-create", "--base-token", job.BaseToken, "--table-id", job.Tables[kind]},
			map[string]any{"create_records": creates[start:end]})
		if err != nil {
			return err
		}
		ids, _ := data["record_id_list"].([]any)
		if len(ids) != end-start {
			return errors.New("批量建记录响应数量不一致，继续任务时将重新查重")
		}
		for i, value := range ids {
			materialID := newItems[start+i].MaterialID
			recordID, _ := value.(string)
			if err := s.setItem(id, materialID, func(item *Item) { item.RecordID, item.Status, item.Error = recordID, "recorded", "" }); err != nil {
				return err
			}
		}
	}
	job, _ = s.store.Job(id)
	updates := map[string]any{}
	uploaded := []Item{}
	flush := func() error {
		if len(updates) == 0 {
			return nil
		}
		_, updateErr := s.lark(ctx, []string{"base", "+record-batch-update", "--base-token", job.BaseToken, "--table-id", job.Tables[kind]},
			map[string]any{"update_records": updates})
		// 批量回写失败仍保留附件 token，继续任务时无需重传。
		for _, item := range uploaded {
			if err := s.setItem(id, item.MaterialID, func(i *Item) {
				i.Status, i.Error = "uploaded", ""
				if updateErr != nil {
					i.Status, i.Error = "failed", redact(updateErr.Error())
				}
			}); err != nil {
				return err
			}
		}
		if updateErr != nil {
			s.event(id, Labels[kind]+"记录更新失败："+updateErr.Error())
		}
		updates, uploaded = map[string]any{}, []Item{}
		return nil
	}
	for _, item := range job.Items {
		if item.Kind != kind || item.Status == "uploaded" {
			continue
		}
		m, path, err := s.File(item.MaterialID)
		if err != nil {
			return err
		}
		if item.RecordID == "" {
			return errors.New("物料缺少目标记录 ID")
		}
		// 已返回附件 token 的项在继续任务时不再次上传。
		if item.FileToken == "" {
			relative, _ := filepath.Rel(s.root, path)
			data, err := s.lark(ctx, []string{"base", "+record-upload-attachment", "--base-token", job.BaseToken,
				"--table-id", job.Tables[kind], "--record-id", item.RecordID, "--field-id", "原始文件", "--file", filepath.ToSlash(relative)}, nil)
			if err != nil {
				if saveErr := s.setItem(id, item.MaterialID, func(i *Item) { i.Status, i.Error = "failed", redact(err.Error()) }); saveErr != nil {
					return saveErr
				}
				s.event(id, item.Name+"附件上传失败："+err.Error())
				continue
			}
			token := findFileToken(data)
			if token == "" {
				return errors.New("附件上传响应缺少 file_token")
			}
			if err := s.setItem(id, item.MaterialID, func(i *Item) { i.FileToken = token }); err != nil {
				return err
			}
			item.FileToken = token
		}
		// 上传接口只会追加，成功后移除同一单元格中的旧版本。
		oldTokens := []string{}
		seen := map[string]bool{}
		for _, token := range index[uniqueKey(c, m)].Files {
			if token != "" && token != item.FileToken && !seen[token] {
				oldTokens = append(oldTokens, token)
				seen[token] = true
			}
		}
		if len(oldTokens) > 0 {
			args := []string{"base", "+record-remove-attachment", "--base-token", job.BaseToken,
				"--table-id", job.Tables[kind], "--record-id", item.RecordID, "--field-id", "原始文件", "--yes"}
			for _, token := range oldTokens {
				args = append(args, "--file-token", token)
			}
			if _, err := s.lark(ctx, args, nil); err != nil {
				if saveErr := s.setItem(id, item.MaterialID, func(i *Item) { i.Status, i.Error = "failed", redact(err.Error()) }); saveErr != nil {
					return saveErr
				}
				s.event(id, item.Name+"旧附件移除失败："+err.Error())
				continue
			}
		}
		values := cells(c, m)
		// 附件成功后才写入新哈希，避免把旧附件误判为新版本。
		values["SHA256"] = m.SHA256
		updates[item.RecordID] = values
		uploaded = append(uploaded, item)
		// 每十份附件回写一次哈希与摘要，减少 CLI 冷启动次数。
		if len(updates) == 10 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	s.event(id, Labels[kind]+"本轮写入结束")
	return nil
}

func findFileToken(value any) string {
	switch data := value.(type) {
	case map[string]any:
		// 兼容上传 shortcut 的不同响应层级。
		for _, key := range []string{"file_token", "fileToken"} {
			if token := stringValue(data, key); token != "" {
				return token
			}
		}
		for _, key := range []string{"file_tokens", "fileTokens"} {
			if tokens, ok := data[key].([]any); ok && len(tokens) > 0 {
				if token, ok := tokens[len(tokens)-1].(string); ok {
					return token
				}
			}
		}
		for _, child := range data {
			if token := findFileToken(child); token != "" {
				return token
			}
		}
	case []any:
		for _, child := range data {
			if token := findFileToken(child); token != "" {
				return token
			}
		}
	}
	return ""
}
