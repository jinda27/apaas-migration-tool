package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testDeveloperOrigin = "https://tenant-dev.aedev.feishuapp.cn"

type developerBrowserStub struct {
	calls []bool
}

func (b *developerBrowserStub) Call(_ context.Context, action string, c Connection, payload any) (json.RawMessage, error) {
	if action != "authorization" {
		return nil, fmt.Errorf("unexpected browser action %s", action)
	}
	refresh := payload.(map[string]bool)["refresh"]
	b.calls = append(b.calls, refresh)
	return json.Marshal(map[string]any{
		"origin": testDeveloperOrigin,
		"headers": map[string]string{
			"x-kunlun-token": fmt.Sprintf("token-%d", len(b.calls)), "x-ae-lane": c.Branch,
			"rpc-persist-lane-c-apaas-lane": "lane_sandbox", "cookie": "ignored=template",
		},
		"cookies": []map[string]any{
			{"name": "session", "value": "initial", "domain": "tenant-dev.aedev.feishuapp.cn", "path": "/ae", "secure": true},
			{"name": "other", "value": "other-tenant", "domain": "someone-dev.aedev.feishuapp.cn", "path": "/"},
		},
	})
}

func (*developerBrowserStub) Close() {}

type developerTransportStub func(http.ResponseWriter, *http.Request)

func (f developerTransportStub) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	f(recorder, req)
	if req.Body != nil {
		_ = req.Body.Close()
	}
	response := recorder.Result()
	response.Request = req
	return response, nil
}

func TestOnlineExportUsesGoForPageAndFlowDefinitions(t *testing.T) {
	s, c, runner := testService(t)
	browser := &developerBrowserStub{}
	s.browser = browser
	page := "{\n  \"status_code\": \"0\", \"data\": {\"api_name\": \"internal\", \"page_specific\": {\"dslVersion\": \"original\"}}\n}\n"
	flow := `{"saved_at":9007199254740993,"workflow_json":{"sourceWorkflowID":1875638841337899,"activities":[]}}`
	s.developerTransport = developerTransportStub(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Scheme+"://"+r.URL.Host != testDeveloperOrigin || r.Header.Get("X-Kunlun-Token") != "token-1" ||
			r.Header.Get("X-Ae-Lane") != c.Branch || r.Header.Get("rpc-persist-lane-c-apaas-lane") != "lane_sandbox" {
			t.Errorf("incorrect request authorization for %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/ae/api/v4/builder/namespaces/test__c/describe/pages/list":
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || string(body) != "{}" || r.Header.Get("Cookie") != "session=initial" {
				t.Error("page catalog request changed")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "renewed", Path: "/ae", Secure: true})
			_, _ = io.WriteString(w, `{"status_code":0,"data":{"total":1,"items":[{"api_name":"internal","api_alias":"appPage_alias","terminal_type":"mobile","builder_version":3,"label":[{"language_code":1033,"text":"Page"},{"language_code":2052,"text":"页面"}]}]}}`)
		case "/ae/api/v3/builder/namespaces/test__c/describe/pageinfo/get":
			if r.Method != http.MethodGet || r.URL.Query().Get("api_name") != "internal" || r.Header.Get("Cookie") != "session=renewed" {
				t.Error("page definition request or cookie renewal changed")
			}
			_, _ = io.WriteString(w, page)
		case "/ae/api/v4/describe/namespaces/test__c/automations":
			if r.Method != http.MethodGet || r.URL.RawQuery != "" {
				t.Error("automation catalog must include all flow types")
			}
			_, _ = io.WriteString(w, `{"status_code":"0","data":{"total":1,"items":[{"api_name":"flow_internal","api_alias":"automation_alias","source_workflow_id":"1875638841337899","label":[{"language_code":1033,"text":"Schedule"}],"input_param_infos":[{"api_name":"variable","label":"Parameter"}]}]}}`)
		case "/ae/api/v1/workflow/namespaces/test__c/wf/export":
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || string(body) != `{"source_workflow_id":"1875638841337899"}` ||
				r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cookie") != "session=renewed" {
				t.Error("flow export contract or shared session changed")
			}
			_, _ = fmt.Fprintf(w, "{\"status_code\":\"0\",\"data\": %s}", flow)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"pages", "flows"}, Mode: "online"})
	if err != nil {
		t.Fatal(err)
	}
	job = settled(t, s, job.ID)
	if job.Status != "completed" || len(job.Items) != 2 || runner.uploads != 2 || !reflect.DeepEqual(browser.calls, []bool{false}) {
		t.Fatalf("unexpected export result: %+v, authorization calls: %v", job, browser.calls)
	}
	want := map[string]struct{ name, apiName, source, raw string }{
		"pages": {"页面", "appPage_alias", "api-definition", page},
		"flows": {"Schedule", "automation_alias", "api", flow},
	}
	for _, item := range job.Items {
		material, path, err := s.File(item.MaterialID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		expected := want[item.Kind]
		if err != nil || string(raw) != expected.raw || material.Name != expected.name || material.APIName != expected.apiName ||
			material.Source != expected.source || material.SHA256 != contentHash([]byte(expected.raw)) {
			t.Fatalf("unexpected %s material: %+v (%v)", item.Kind, material, err)
		}
	}
}

func TestDeveloperCollectionResumesLegacyCheckpoints(t *testing.T) {
	s, c, _ := testService(t)
	s.browser = &developerBrowserStub{}
	dir := filepath.Join(s.exports, "job", "downloads", "pages")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	page := func(id string) string {
		return fmt.Sprintf(`{"status_code":"0","data":{"api_name":%q,"page_specific":{"dslVersion":"original"}}}`, id)
	}
	first, second := filepath.Join(dir, "one.definition.json"), filepath.Join(dir, "two.definition.json")
	for filename, raw := range map[string]string{first: page("one"), second: `{"corrupt":true}`} {
		if err := os.WriteFile(filename, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Node 的断点以内部 ID 为索引，记录绝对路径和原始文件校验值。
	legacy := fmt.Sprintf(`{"one":{"name":"One","apiName":"one","path":%q,"method":"api","source":"api-definition","sha256":%q,"metadata":{"api_name":"one"}},"two":{"name":"Two","apiName":"two","path":%q,"method":"api","source":"api-definition","sha256":%q}}`,
		first, contentHash([]byte(page("one"))), second, contentHash([]byte(page("two"))))
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	var fetched []string
	s.developerTransport = developerTransportStub(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pages/list") {
			_, _ = io.WriteString(w, `{"data":{"total":2,"items":[{"api_name":"one","name":"One","terminal_type":"pc","builder_version":3},{"api_name":"two","name":"Two","terminal_type":"web","builder_version":3}]}}`)
			return
		}
		id := r.URL.Query().Get("api_name")
		fetched = append(fetched, id)
		_, _ = io.WriteString(w, page(id))
	})
	session := &developerSession{service: s, connection: c}
	materials, err := s.collectSessionAPI(context.Background(), session, "pages", "job")
	if err != nil || len(materials) != 2 || !reflect.DeepEqual(fetched, []string{"two"}) {
		t.Fatalf("legacy checkpoint was not reused correctly: %+v %v %v", materials, fetched, err)
	}
	again, err := s.collectSessionAPI(context.Background(), session, "pages", "job")
	if err != nil || !reflect.DeepEqual(materials, again) || !reflect.DeepEqual(fetched, []string{"two"}) {
		t.Fatalf("Go checkpoint was not reused correctly: %+v %v %v", again, fetched, err)
	}
}

func TestLatestSuccessfulExportReplacesSelectedMaterials(t *testing.T) {
	s, c, runner := testService(t)
	s.browser = &developerBrowserStub{}
	start := func(kind, mode string) Job {
		t.Helper()
		job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{kind}, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		return settled(t, s, job.ID)
	}
	flow, err := s.Import(c.ID, "flows", "flow.json", strings.NewReader(`{"apiName":"kept_flow"}`))
	if err != nil {
		t.Fatal(err)
	}
	if job := start("flows", "imported"); job.Status != "completed" {
		t.Fatalf("seed flow export failed: %+v", job)
	}

	setPages := func(version string, ids ...string) {
		s.developerTransport = developerTransportStub(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/pages/list") {
				items := make([]map[string]any, 0, len(ids))
				for _, id := range ids {
					items = append(items, map[string]any{"api_name": id, "label": id, "terminal_type": "pc", "builder_version": 3})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "total": len(items)}})
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":{"api_name":%q,"page_specific":{"version":%q}}}`, r.URL.Query().Get("api_name"), version)
		})
	}
	setPages("old", "common_page", "removed_page")
	if job := start("pages", "online"); job.Status != "completed" {
		t.Fatalf("initial pages export failed: %+v", job)
	}
	index := func(materials []Material) map[string]string {
		result := map[string]string{}
		for _, material := range materials {
			result[material.APIName] = material.ID
		}
		return result
	}
	before := index(s.Materials(c.ID))
	if len(before) != 3 || before["kept_flow"] != flow.ID {
		t.Fatalf("unexpected initial materials: %v", before)
	}

	setPages("new", "common_page")
	runner.failUpload = true
	if job := start("pages", "online"); job.Status != "failed" {
		t.Fatalf("expected write failure: %+v", job)
	}
	if afterFailure := index(s.Materials(c.ID)); !reflect.DeepEqual(afterFailure, before) {
		t.Fatalf("failed export changed visible materials: %v", afterFailure)
	}

	if job := start("pages", "online"); job.Status != "completed" {
		t.Fatalf("replacement export failed: %+v", job)
	}
	after := index(s.Materials(c.ID))
	if len(after) != 2 || after["common_page"] == "" || after["common_page"] == before["common_page"] || after["kept_flow"] != flow.ID {
		t.Fatalf("completion should replace only the selected kind: %v", after)
	}
	if _, _, err := s.File(before["removed_page"]); err != nil {
		t.Fatalf("old local artifact should still be readable: %v", err)
	}

	raw, err := os.ReadFile(s.store.file)
	if err != nil {
		t.Fatal(err)
	}
	var oldState map[string]json.RawMessage
	if err := json.Unmarshal(raw, &oldState); err != nil {
		t.Fatal(err)
	}
	delete(oldState, "catalog")
	legacy, err := json.Marshal(oldState)
	if err != nil {
		t.Fatal(err)
	}
	legacyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacyDir, "state.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewStore(legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	if migrated := index(recovered.CurrentMaterials(c.ID)); !reflect.DeepEqual(migrated, after) {
		t.Fatalf("legacy history did not recover the latest snapshot: %v", migrated)
	}
	setPages("empty")
	if job := start("pages", "online"); job.Status != "completed" {
		t.Fatalf("a confirmed empty directory should complete: %+v", job)
	}
	if cleared := index(s.Materials(c.ID)); len(cleared) != 1 || cleared["kept_flow"] != flow.ID {
		t.Fatalf("empty result should clear only the selected kind: %v", cleared)
	}
}

func TestDeveloperRejectsIncompleteCatalogAndMismatchedFlow(t *testing.T) {
	raw := json.RawMessage(`{"total":2,"items":[{"api_name":"flow","name":"Flow","source_workflow_id":"123"}]}`)
	if _, err := decodeDeveloperCatalog(raw); err == nil {
		t.Fatal("accepted an incomplete catalog")
	}
	s, c, _ := testService(t)
	s.browser = &developerBrowserStub{}
	s.developerTransport = developerTransportStub(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"workflow_json":{"sourceWorkflowID":"999"}}}`)
	})
	session := &developerSession{service: s, connection: c}
	if _, err := session.definition(context.Background(), "flows", developerAsset{workflowID: "123"}); err == nil {
		t.Fatal("accepted a definition for another workflow")
	}
}

func TestOnlineObjectExportUsesGoForPaginatedDirectory(t *testing.T) {
	s, c, runner := testService(t)
	browser := &developerBrowserStub{}
	s.browser = browser
	var offsets []string
	var metadataNames []string
	s.developerTransport = developerTransportStub(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ae/api/v1/describe/namespaces/test__c/objects" {
			t.Errorf("unexpected object catalog request %s %s", r.Method, r.URL.Path)
		}
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)
		switch offset {
		case "":
			_, _ = io.WriteString(w, `{"data":{"total":2,"items":[{"api_name":"internal_object_a","api_alias":"object_a","label":"Object A"}]}}`)
		case "1":
			_, _ = io.WriteString(w, `{"data":{"total":2,"items":[{"api_name":"object_b","label":"Object B"}]}}`)
		default:
			t.Errorf("unexpected offset %s", offset)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	s.apaas.client.Transport = developerTransportStub(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/v1/appToken" {
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"T:test"}}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/data/v1/namespaces/test__c/meta/objects/") || r.Header.Get("Authorization") != "T:test" {
			t.Errorf("unexpected object metadata request %s", r.URL.Path)
		}
		apiName := filepath.Base(r.URL.Path)
		metadataNames = append(metadataNames, apiName)
		// 公开接口仅接受 API 名称，内部标识返回对象不存在。
		if apiName != "object_a" && apiName != "object_b" {
			_, _ = io.WriteString(w, `{"code":"object_not_found","msg":"对象不存在"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"apiName":%q,"fields":[{"apiName":"field"}]}}`, apiName)
	})
	job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"objects"}, Mode: "online"})
	if err != nil {
		t.Fatal(err)
	}
	job = settled(t, s, job.ID)
	if job.Status != "completed" || len(job.Items) != 2 || runner.uploads != 2 ||
		!reflect.DeepEqual(browser.calls, []bool{false}) || !reflect.DeepEqual(offsets, []string{"", "1"}) ||
		!reflect.DeepEqual(metadataNames, []string{"object_a", "object_b"}) {
		t.Fatalf("unexpected object export: %+v, auth calls %v, offsets %v, metadata names %v",
			job, browser.calls, offsets, metadataNames)
	}
}

func TestDeveloperSessionRefreshAndRejectsRedirects(t *testing.T) {
	s, c, _ := testService(t)
	browser := &developerBrowserStub{}
	s.browser = browser
	var paths []string
	s.developerTransport = developerTransportStub(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/refresh":
			if r.Header.Get("X-Kunlun-Token") == "token-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"data":{}}`)
		case "/expired":
			w.WriteHeader(http.StatusUnauthorized)
		case "/denied":
			w.WriteHeader(http.StatusForbidden)
		case "/redirect":
			w.Header().Set("Location", "https://unrelated.example/receive")
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			t.Errorf("followed an unexpected redirect to %s", r.URL.Host)
		}
	})
	session := &developerSession{service: s, connection: c}
	ctx := context.Background()
	if _, _, err := session.request(ctx, http.MethodGet, "/denied", nil); !errors.Is(err, errDeveloperDenied) {
		t.Fatalf("expected permission error, got %v", err)
	}
	if _, _, err := session.request(ctx, http.MethodGet, "/refresh", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.request(ctx, http.MethodGet, "/expired", nil); !errors.Is(err, errDeveloperLogin) {
		t.Fatalf("expected expired session, got %v", err)
	}
	if _, _, err := session.request(ctx, http.MethodGet, "/redirect", nil); err == nil {
		t.Fatal("cross-origin redirect was accepted")
	}
	if !reflect.DeepEqual(browser.calls, []bool{false, true}) ||
		!reflect.DeepEqual(paths, []string{"/denied", "/refresh", "/refresh", "/expired", "/redirect"}) {
		t.Fatalf("authorization calls %v, requests %v", browser.calls, paths)
	}
}

func TestDeveloperExportCancelsInFlightHTTP(t *testing.T) {
	s, c, _ := testService(t)
	s.browser = &developerBrowserStub{}
	started := make(chan struct{})
	s.developerTransport = developerTransportStub(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	job, err := s.Start(JobInput{ConnectionID: c.ID, Kinds: []string{"pages"}, Mode: "online"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("export did not start the Go HTTP request")
	}
	if err := s.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	if job := settled(t, s, job.ID); job.Status != "cancelled" || len(job.Items) != 0 {
		t.Fatalf("unexpected state after HTTP cancellation: %+v", job)
	}
}
