package exporter

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRunner struct {
	mu               sync.Mutex
	records          map[string]map[string]any
	creates, uploads int
	removals         int
	baseNames        []string
	baseGets         []string
	updates          int
	failUpdate       bool
	failUpload       bool
	missingIDs       bool
}

func argument(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeRunner) Run(ctx context.Context, dir, name string, args []string, _ []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name != "lark-cli" || argument(args, "--as") != "user" {
		return nil, errors.New("expected user identity")
	}
	if f.records == nil {
		f.records = map[string]map[string]any{}
	}
	data := map[string]any{}
	var input map[string]any
	if file := argument(args, "--json"); strings.HasPrefix(file, "@") {
		raw, err := os.ReadFile(filepath.Join(dir, strings.TrimPrefix(file, "@")))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
	}
	switch args[1] {
	case "+base-create":
		f.baseNames = append(f.baseNames, argument(args, "--name"))
		data["base"] = map[string]string{"base_token": "new-base", "url": "https://tenant.feishu.cn/base/new-base"}
	case "+base-get":
		f.baseGets = append(f.baseGets, argument(args, "--base-token"))
	case "+table-list":
		list := []map[string]any{}
		for _, kind := range Kinds {
			list = append(list, map[string]any{"id": "tbl-" + kind, "name": Labels[kind]})
		}
		data["tables"] = list
	case "+field-list":
		kind := strings.TrimPrefix(argument(args, "--table-id"), "tbl-")
		data["fields"] = fields(kind)
	case "+record-list":
		stream := []byte{}
		for id, values := range f.records {
			row := map[string]any{"record_id": id}
			for key, value := range values {
				row[key] = value
			}
			raw, _ := json.Marshal(row)
			stream = append(stream, append(raw, '\n')...)
		}
		file := filepath.Join(dir, argument(args, "--output"))
		if err := os.WriteFile(file, stream, 0600); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"manifest_version": "v1", "record_file": file, "has_more": false})
	case "+record-batch-create":
		ids := []string{}
		for _, value := range input["create_records"].([]any) {
			f.creates++
			id := fmt.Sprintf("rec-%d", f.creates)
			f.records[id] = value.(map[string]any)
			ids = append(ids, id)
		}
		data["record_id_list"] = ids
		// 模拟服务端已创建记录、客户端未取得记录 ID 的不确定结果。
		if f.missingIDs {
			f.missingIDs = false
			data["record_id_list"] = []string{}
		}
	case "+record-upload-attachment":
		if f.failUpload {
			f.failUpload = false
			return nil, errors.New("injected upload failure")
		}
		file := filepath.Join(dir, argument(args, "--file"))
		if _, err := os.ReadFile(file); err != nil {
			return nil, err
		}
		f.uploads++
		token := fmt.Sprintf("file-%d", f.uploads)
		id := argument(args, "--record-id")
		attachments, _ := f.records[id]["原始文件"].([]map[string]string)
		f.records[id]["原始文件"] = append(attachments, map[string]string{"file_token": token})
		data["file_tokens"] = []string{token}
	case "+record-remove-attachment":
		id := argument(args, "--record-id")
		remove := map[string]bool{}
		for i, arg := range args {
			if arg == "--file-token" && i+1 < len(args) {
				remove[args[i+1]] = true
			}
		}
		attachments, _ := f.records[id]["原始文件"].([]map[string]string)
		kept := []map[string]string{}
		for _, attachment := range attachments {
			if remove[attachment["file_token"]] {
				f.removals++
				continue
			}
			kept = append(kept, attachment)
		}
		f.records[id]["原始文件"] = kept
	case "+record-batch-update":
		f.updates++
		if f.failUpdate {
			f.failUpdate = false
			return nil, errors.New("injected update failure")
		}
		for id, values := range input["update_records"].(map[string]any) {
			for key, value := range values.(map[string]any) {
				f.records[id][key] = value
			}
		}
	default:
		return nil, fmt.Errorf("unexpected command %s", args[1])
	}
	return json.Marshal(map[string]any{"ok": true, "data": data})
}

func TestAttachmentBatchFailureKeepsTokensAndHashes(t *testing.T) {
	s, c, runner := testService(t)
	runner.failUpdate = true
	// 覆盖多批上传中首批更新失败、后续成功和失败项续传。
	for i := range 25 {
		api := fmt.Sprintf("page_%d", i)
		if _, err := s.Import(c.ID, "pages", api+".json", strings.NewReader(`{"apiName":"`+api+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "imported"})
	if err != nil {
		t.Fatal(err)
	}
	first := settled(t, s, job.ID)
	if first.Status != "partial" || runner.updates != 3 || runner.uploads != 25 {
		t.Fatalf("unexpected batch result: %+v", first)
	}
	failed := 0
	for _, item := range first.Items {
		if item.Status == "failed" {
			failed++
			if item.FileToken == "" || runner.records[item.RecordID]["SHA256"] != nil {
				t.Fatal("failed batch lost token or wrote an unconfirmed hash")
			}
		}
	}
	if failed != 10 {
		t.Fatalf("expected 10 failed updates, got %d", failed)
	}
	if err := s.Retry(job.ID); err != nil {
		t.Fatal(err)
	}
	done := settled(t, s, job.ID)
	if done.Status != "completed" || runner.updates != 4 || runner.uploads != 25 || runner.creates != 25 {
		t.Fatalf("retry duplicated attachments or records: %+v", done)
	}
}

func TestExistingDuplicateAttachmentsAreCollapsed(t *testing.T) {
	s, c, runner := testService(t)
	if _, err := s.Import(c.ID, "pages", "page.json", strings.NewReader(`{"apiName":"page_a"}`)); err != nil {
		t.Fatal(err)
	}
	first, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "imported"})
	if err != nil {
		t.Fatal(err)
	}
	first = settled(t, s, first.ID)
	recordID := first.Items[0].RecordID
	attachments, _ := runner.records[recordID]["原始文件"].([]map[string]string)
	// 模拟历史重导遗留的重复附件。
	runner.records[recordID]["原始文件"] = append(attachments, map[string]string{"file_token": "file-legacy"})

	next, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "imported"})
	if err != nil {
		t.Fatal(err)
	}
	next = settled(t, s, next.ID)
	attachments, _ = runner.records[recordID]["原始文件"].([]map[string]string)
	if next.Status != "completed" || runner.uploads != 1 || runner.removals != 1 || len(attachments) != 1 {
		t.Fatalf("duplicate attachments were not collapsed: job=%+v attachments=%v", next, attachments)
	}
}

func testService(t *testing.T) (*Service, Connection, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	s, err := New(root, filepath.Join(root, "state"), filepath.Join(root, "exports"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	runner := &fakeRunner{}
	s.runner = runner
	c, err := s.Save(ConnectionInput{Connection: Connection{
		AppURL: "https://tenant.feishu.cn/ae/app_manage/test__c/overview", AppName: "测试应用",
		BaseURL: "https://tenant.feishu.cn/base/test-base", ClientID: "test-client",
	}, ClientSecret: "private-test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, runner
}

func TestConnectionCredentialViewAndUpdate(t *testing.T) {
	s, c, _ := testService(t)
	view := s.ConnectionView(c)
	if view.ClientID != "test-client" || !view.ClientSecretConfigured {
		t.Fatalf("credential status not exposed: %+v", view)
	}

	// 编辑其它字段时 Secret 留空，保留当前进程中的凭证。
	c.AppName = "修改后的应用"
	saved, err := s.Save(ConnectionInput{Connection: c})
	if err != nil || !s.ConnectionView(saved).ClientSecretConfigured {
		t.Fatalf("empty secret did not preserve credentials: %v", err)
	}

	// 修改 Client ID 时必须同时提交新 Secret。
	saved.ClientID = "changed-client"
	if _, err := s.Save(ConnectionInput{Connection: saved}); err == nil {
		t.Fatal("client ID changed without a new secret")
	}
	saved, err = s.Save(ConnectionInput{Connection: saved, ClientSecret: "changed-secret"})
	if err != nil {
		t.Fatal(err)
	}
	view = s.ConnectionView(saved)
	if view.ClientID != "changed-client" || !view.ClientSecretConfigured {
		t.Fatalf("updated credential status not exposed: %+v", view)
	}
	raw, _ := os.ReadFile(s.store.file)
	if !strings.Contains(string(raw), "changed-client") || strings.Contains(string(raw), "changed-secret") {
		t.Fatal("client ID or secret persistence is incorrect")
	}
}

func settled(t *testing.T, s *Service, id string) Job {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		job, _ := s.Job(id)
		s.mu.Lock()
		_, running := s.running[id]
		s.mu.Unlock()
		if !active(job.Status) && !running {
			return job
		}
		select {
		case <-timer.C:
			t.Fatalf("job did not settle: %+v", job)
		case <-tick.C:
		}
	}
}

func TestFirstExportCreatesAndReusesInstanceBase(t *testing.T) {
	s, c, runner := testService(t)
	c.AppName, c.BaseURL = "", " "
	c, err := s.Save(ConnectionInput{Connection: c})
	if err != nil || c.BaseURL != "" {
		t.Fatalf("instance should accept an empty target: %v", err)
	}
	input := JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "imported"}
	empty, err := s.Start(input)
	if err != nil {
		t.Fatal(err)
	}
	empty = settled(t, s, empty.ID)
	if empty.Status != "failed" || empty.BaseURL != "" || len(runner.baseNames) != 0 {
		t.Fatalf("export without materials should not create a Base: %+v", empty)
	}

	if _, err := s.Import(c.ID, "pages", "page.json", strings.NewReader(`{"apiName":"page_a"}`)); err != nil {
		t.Fatal(err)
	}
	first, err := s.Start(input)
	if err != nil {
		t.Fatal(err)
	}
	first = settled(t, s, first.ID)
	const expectedURL = "https://tenant.feishu.cn/base/new-base"
	if first.Status != "completed" || first.AppName != c.Namespace || first.BaseURL != expectedURL ||
		len(runner.baseNames) != 1 || runner.baseNames[0] != c.Namespace+"物料数据" {
		t.Fatalf("unexpected first export: %+v, created Bases: %v", first, runner.baseNames)
	}

	reopened, err := NewStore(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reopened.Connection(c.ID)
	if !ok || saved.BaseURL != expectedURL {
		t.Fatal("the first export did not persist the instance target")
	}

	if err := s.Retry(empty.ID); err != nil {
		t.Fatal(err)
	}
	retried := settled(t, s, empty.ID)
	next, err := s.Start(input)
	if err != nil {
		t.Fatal(err)
	}
	next = settled(t, s, next.ID)
	if retried.Status != "completed" || retried.BaseURL != expectedURL || next.Status != "completed" || next.BaseURL != expectedURL ||
		len(runner.baseNames) != 1 || strings.Join(runner.baseGets, ",") != "new-base,new-base" {
		t.Fatalf("retry or later export did not reuse the first Base: retry %+v, next %+v, created %v, opened %v",
			retried, next, runner.baseNames, runner.baseGets)
	}
}

func TestFourLaneProgressAggregatesSourceMaterials(t *testing.T) {
	s, c, _ := testService(t)
	for _, kind := range Kinds {
		raw := `{"apiName":"` + kind + `_item"}`
		if _, err := s.Import(c.ID, kind, kind+".json", strings.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: Kinds, Mode: "imported"})
	if err != nil {
		t.Fatal(err)
	}
	done := settled(t, s, job.ID)
	expected := map[string][2]int{
		"pages": {1, 1}, "flows": {1, 1}, "source": {2, 2}, "objects": {1, 1},
	}
	if len(done.Progress) != 4 {
		t.Fatalf("expected four progress lanes, got %+v", done.Progress)
	}
	for _, progress := range done.Progress {
		counts, ok := expected[progress.Key]
		if !ok || progress.Current != counts[0] || progress.Total != counts[1] || progress.Status != "completed" {
			t.Fatalf("unexpected progress lane: %+v", progress)
		}
	}
}

func TestOnlineCollectionFallsBackToPublishedMaterials(t *testing.T) {
	s, c, _ := testService(t)
	material, err := s.Import(c.ID, "objects", "object.json", strings.NewReader(`{"apiName":"object_a"}`))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "fallback-job", ConnectionID: c.ID, Kinds: []string{"objects"}, Mode: "online",
		Status: "collecting", Progress: newProgress([]string{"objects"}), SourceErrors: map[string]string{},
		Items: []Item{}, Events: []Event{}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.store.Change(func(data *state) {
		data.Jobs[job.ID] = job
		data.catalogFor(c.ID).add(material)
	}); err != nil {
		t.Fatal(err)
	}
	materials, err := s.useCachedCollection(job.ID, c, "objects", nil, errors.New("credentials unavailable"), s.laneReporter(job.ID, "objects"))
	if err != nil || len(materials) != 1 || materials[0].ID != material.ID {
		t.Fatalf("published material fallback failed: %+v, %v", materials, err)
	}
	saved, _ := s.Job(job.ID)
	if saved.Progress[3].Current != 1 || saved.Progress[3].Total != 1 ||
		!strings.Contains(saved.Events[len(saved.Events)-1].Message, "最近一次成功采集物料") {
		t.Fatalf("fallback progress was not persisted: %+v", saved)
	}
}

func TestImportedExportResumeAndNewVersion(t *testing.T) {
	s, c, runner := testService(t)
	runner.failUpdate = true
	for _, api := range []string{"page_a", "page_b"} {
		if _, err := s.Import(c.ID, "pages", api+".json", strings.NewReader(`{"apiName":"`+api+`","name":"测试页面"}`)); err != nil {
			t.Fatal(err)
		}
	}
	job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "imported"})
	if err != nil {
		t.Fatal(err)
	}
	first := settled(t, s, job.ID)
	if first.Status != "failed" || runner.creates != 2 || runner.uploads != 2 {
		t.Fatalf("expected failed batch export: %+v", first)
	}
	if err := s.Retry(job.ID); err != nil {
		t.Fatal(err)
	}
	done := settled(t, s, job.ID)
	if done.Status != "completed" || runner.creates != 2 || runner.uploads != 2 {
		t.Fatalf("resume duplicated writes: %+v", done)
	}
	archive, err := s.Archive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if len(z.File) != 3 {
		t.Fatalf("unexpected archive files: %d", len(z.File))
	}
	raw, _ := os.ReadFile(s.store.file)
	if strings.Contains(string(raw), "private-test-secret") || !strings.Contains(string(raw), "test-client") {
		t.Fatal("credential persistence policy violated")
	}
	restarted, err := NewStore(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := restarted.Job(job.ID)
	if saved.Status != "completed" || saved.Items[0].RecordID == "" || saved.Items[0].FileToken == "" {
		t.Fatal("completed progress lost after restart")
	}
	newMaterial, err := s.Import(c.ID, "pages", "page_a.json", strings.NewReader(`{"apiName":"page_a","name":"新版本"}`))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟部分目录采集失败后源内容变更，继续时不能产生相同幂等键的两个项。
	if err := s.changeJob(job.ID, func(j *Job) { j.Status = "partial"; j.SourceErrors["pages"] = "directory incomplete" }); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(job.ID); err != nil {
		t.Fatal(err)
	}
	done = settled(t, s, job.ID)
	found := false
	for _, item := range done.Items {
		found = found || item.MaterialID == newMaterial.ID
	}
	if !found || len(done.Items) != 2 || runner.creates != 2 || runner.uploads != 3 || done.Status != "completed" {
		t.Fatalf("changed source was not replaced correctly: %+v", done)
	}
	attachments, _ := runner.records[done.Items[0].RecordID]["原始文件"].([]map[string]string)
	if len(attachments) != 1 || runner.removals != 1 {
		t.Fatalf("old attachment was not replaced: attachments=%v removals=%d", attachments, runner.removals)
	}
}

func TestImportValidationIntegrityAndStoreRollback(t *testing.T) {
	s, c, _ := testService(t)
	if _, err := s.Import(c.ID, "objects", "bad.json", strings.NewReader(`{`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	m, err := s.Import(c.ID, "pages", "../../page.json", strings.NewReader(`{"name":"页面"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := s.File(m.ID)
	if err != nil || !strings.HasPrefix(path, s.exports+string(os.PathSeparator)) {
		t.Fatal("unsafe import path")
	}
	if err := os.WriteFile(path, []byte(`{"modified":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.File(m.ID); err == nil {
		t.Fatal("changed bytes accepted")
	}
	if err := os.Mkdir(s.store.file+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Change(func(data *state) { delete(data.Connections, c.ID) }); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if _, ok := s.store.Connection(c.ID); !ok {
		t.Fatal("failed update changed in-memory state")
	}
}

func TestImportedJSONMaterialIdentity(t *testing.T) {
	s, c, _ := testService(t)
	cases := []struct {
		kind, raw, apiName, name string
		fieldCount               int
	}{
		{"objects", `{"object":{"api_name":"object_api","label":[{"language_code":2052,"text":"对象名称"}],"type":"object"},"fields":[{},{}]}`, "object_api", "对象名称", 2},
		{"functions", `{"apiName":"function_api","label":{"zh_CN":"函数名称"}}`, "function_api", "函数名称", 0},
		{"components", `{"apiName":"component_api","name":"组件名称"}`, "component_api", "组件名称", 0},
	}
	// 文件名不包含身份时仍从原始定义读取名称，保持内容字节不变。
	for _, test := range cases {
		m, err := s.Import(c.ID, test.kind, "download.json", strings.NewReader(test.raw))
		if err != nil {
			t.Fatal(err)
		}
		if m.APIName != test.apiName || m.Name != test.name || m.FieldCount != test.fieldCount ||
			m.SHA256 != contentHash([]byte(test.raw)) {
			t.Fatalf("imported definition lost identity or content: %+v", m)
		}
	}
}

func TestRemoteWriteFailuresResumeWithoutDuplicates(t *testing.T) {
	for _, scenario := range []string{"upload", "missing-record-ids"} {
		t.Run(scenario, func(t *testing.T) {
			s, c, runner := testService(t)
			runner.failUpload = scenario == "upload"
			runner.missingIDs = scenario == "missing-record-ids"
			if _, err := s.Import(c.ID, "pages", "page.json", strings.NewReader(`{"apiName":"page"}`)); err != nil {
				t.Fatal(err)
			}
			job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "imported"})
			if err != nil {
				t.Fatal(err)
			}
			first := settled(t, s, job.ID)
			if first.Status != "failed" || runner.creates != 1 {
				t.Fatalf("unexpected first failure: %+v", first)
			}
			if err := s.Retry(job.ID); err != nil {
				t.Fatal(err)
			}
			done := settled(t, s, job.ID)
			if done.Status != "completed" || runner.creates != 1 || runner.uploads != 1 {
				t.Fatalf("retry duplicated remote writes: %+v", done)
			}
		})
	}
}

func TestRestartInterruptedAndLegacyMaps(t *testing.T) {
	dir := t.TempDir()
	raw := `{"jobs":{"j":{"id":"j","status":"writing","items":[{"materialId":"m","recordId":"rec","fileToken":"file"}]}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := store.Job("j")
	if job.Status != "interrupted" || job.Tables == nil || job.SourceErrors == nil || job.Items[0].FileToken != "file" {
		t.Fatal("restart did not preserve progress")
	}
	if err := store.Change(func(data *state) { data.Connections["c"] = Connection{ID: "c"} }); err != nil {
		t.Fatal(err)
	}
}

func TestAPaaSTokenCacheFlowAndPermission(t *testing.T) {
	var authCalls, deniedCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/v1/appToken" {
			authCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"T:test"}}`))
			return
		}
		if r.Header.Get("Authorization") != "T:test" {
			t.Error("missing app token")
		}
		if strings.HasSuffix(r.URL.Path, "/denied") {
			deniedCalls.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"permission_denied","msg":"not authorized"}`))
			return
		}
		if strings.Contains(r.URL.Path, "/flow/") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"workflow":"{\"steps\":[]}"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"apiName":"object_a","fields":[{"apiName":"field_a"}]}}`))
	}))
	defer server.Close()
	api := NewAPaaS()
	api.baseURL = server.URL
	api.Set("c", "client", "secret")
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := api.Object(context.Background(), Connection{ID: "c", Namespace: "test__c"}, "object_a"); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if authCalls.Load() != 1 {
		t.Fatal("token was not cached")
	}
	flow, err := api.Flow(context.Background(), Connection{ID: "c", Namespace: "test__c"}, "flow_a")
	if err != nil || string(flow) != `{"steps":[]}` {
		t.Fatal("flow definition lost")
	}
	if _, err := api.Object(context.Background(), Connection{ID: "c"}, "denied"); err == nil || deniedCalls.Load() != 1 {
		t.Fatal("permission failure retried or ignored")
	}
}

type blockingBrowser struct{ entered chan struct{} }

func (b *blockingBrowser) Call(ctx context.Context, _ string, _ Connection, _ any) (json.RawMessage, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*blockingBrowser) Close() {}

func TestCancelAndConcurrentStart(t *testing.T) {
	s, c, _ := testService(t)
	browser := &blockingBrowser{entered: make(chan struct{})}
	s.browser = browser
	input := JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "online"}
	job, err := s.Start(input)
	if err != nil {
		t.Fatal(err)
	}
	<-browser.entered
	if _, err := s.Start(input); err == nil {
		t.Fatal("concurrent job accepted")
	}
	if err := s.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	done := settled(t, s, job.ID)
	if done.Status != "cancelled" {
		t.Fatalf("unexpected cancelled status: %s", done.Status)
	}
	if _, err := s.Archive(job.ID); err != nil {
		t.Fatal("empty failed export has no manifest archive")
	}
}

func TestProjectMaterialsIncludeSharedCodeAndKeepBoundaries(t *testing.T) {
	s, c, _ := testService(t)
	project := t.TempDir()
	files := map[string]string{
		"package.json":                                                `{}`,
		"functions/nodejs/package.json":                               `{}`,
		"functions/nodejs/_shared/helper.js":                          `module.exports = {}`,
		"functions/nodejs/first/index.meta.json":                      `{"apiName":"first","label":[{"language_code":2052,"text":"第一个函数"}]}`,
		"functions/nodejs/first/index.js":                             `module.exports = () => 1`,
		"functions/nodejs/_common/controllers/nested/index.meta.json": `{"apiName":"nested"}`,
		"functions/nodejs/_common/controllers/nested/index.js":        `module.exports = () => 2`,
		"functions/nodejs_20/package.json":                            `{}`,
		"functions/nodejs_20/newer/index.meta.json":                   `{"apiName":"newer"}`,
		"functions/nodejs_20/newer/index.js":                          `module.exports = () => 3`,
		"components/package.json":                                     `{}`,
		"components/Picker/index.meta.json":                           `{"apiName":"Picker"}`,
		"components/Picker/index.tsx":                                 `export default null`,
		".ae/private.json":                                            `{"private":"value"}`,
		"functions/nodejs/.env":                                       `SECRET=value`,
	}
	for relative, raw := range files {
		// 夹具覆盖实际运行时目录、嵌套函数和公共依赖结构。
		file := filepath.Join(project, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	functions, err := s.collectProject(c, "functions", project)
	if err != nil || len(functions) != 3 {
		t.Fatalf("function boundaries incorrect: %d, %v", len(functions), err)
	}
	for _, m := range functions {
		_, file, err := s.File(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		archive, err := zip.OpenReader(file)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, entry := range archive.File {
			names[entry.Name] = true
			if strings.Contains(entry.Name, ".ae/") || strings.Contains(entry.Name, ".env") || strings.HasPrefix(entry.Name, "components/") {
				t.Fatalf("unrelated/private file included: %s", entry.Name)
			}
		}
		_ = archive.Close()
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		// 源码包重新导入时保留嵌套路径、运行环境和名称。
		imported, err := s.Import(c.ID, "functions", "download.zip", bytes.NewReader(raw))
		if err != nil || imported.APIName != m.APIName || imported.Name != m.Name ||
			imported.ProjectPath != m.ProjectPath || imported.Runtime != m.Runtime || imported.SHA256 != m.SHA256 {
			t.Fatalf("source package metadata changed on import: %+v, %v", imported, err)
		}
		if !names[m.ProjectPath+"/index.js"] || !names["package.json"] ||
			!names["functions/"+m.Runtime+"/package.json"] {
			t.Fatalf("function files or dependencies missing: %+v", m)
		}
		if m.Runtime == "nodejs" && !names["functions/nodejs/_shared/helper.js"] {
			t.Fatal("shared helper excluded")
		}
		if m.APIName == "first" && (m.Name != "第一个函数" || names["functions/nodejs/_common/controllers/nested/index.js"]) {
			t.Fatal("nested function leaked into another package")
		}
	}
	components, err := s.collectProject(c, "components", project)
	if err != nil || len(components) != 1 || components[0].APIName != "Picker" {
		t.Fatalf("component boundaries incorrect: %+v, %v", components, err)
	}
}

func TestDownloadedProjectMaterialCounts(t *testing.T) {
	project := os.Getenv("APAAS_VERIFICATION_PROJECT")
	if project == "" {
		t.Skip("requires a downloaded project fixture")
	}
	s, c, _ := testService(t)
	expected := map[string]int{}
	if err := filepath.WalkDir(project, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(project, file)
		// 实测预期数量直接来自工程定义，兼容后续应用增删物料。
		if ignoredFile(relative) && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.Name() == "index.meta.json" {
			kind, _ := projectScope(relative)
			expected[kind]++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"functions", "components"} {
		materials, err := s.collectProject(c, kind, project)
		if err != nil {
			t.Fatal(err)
		}
		if len(materials) != expected[kind] {
			t.Fatalf("%s count: got %d, expected %d", kind, len(materials), expected[kind])
		}
		t.Logf("%s: %d 个独立源码包，原文件哈希已核验", kind, len(materials))
		for _, material := range materials {
			if _, _, err := s.File(material.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}
