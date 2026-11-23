package httpapi

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"path/filepath"
	"time"

	"github.com/jinda27/apaas-migration-tool/server/internal/exporter"
)

func registerExport(mux *http.ServeMux, service *exporter.Service) {
	fail := func(w http.ResponseWriter, err error) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	decode := func(w http.ResponseWriter, r *http.Request, value any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		// 拒绝无效配置与超出限制的请求体。
		if err := decoder.Decode(value); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求数据无效或过大"})
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/workspace", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, service.Snapshot())
	})
	mux.HandleFunc("POST /api/connections", func(w http.ResponseWriter, r *http.Request) {
		var input exporter.ConnectionInput
		if !decode(w, r, &input) {
			return
		}
		c, err := service.Save(input)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, service.ConnectionView(c))
	})
	mux.HandleFunc("POST /api/connections/{id}/check", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		result, err := service.Probe(ctx, r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /api/connections/{id}/authorize", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Kind string `json:"kind"`
		}
		if !decode(w, r, &input) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 110*time.Second)
		defer cancel()
		result, err := service.Authorize(ctx, r.PathValue("id"), input.Kind)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /api/connections/{id}/qrcode", func(w http.ResponseWriter, r *http.Request) {
		path, err := service.QRCode(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		http.ServeFile(w, r, path)
	})
	mux.HandleFunc("GET /api/connections/{id}/materials", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, service.Materials(r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/materials/import", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
		// multipart 临时文件由系统目录承载，处理结束立即清理。
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "导入请求无效，单次上传不能超过 128MB"})
			return
		}
		defer r.MultipartForm.RemoveAll()
		files := r.MultipartForm.File["files"]
		if len(files) == 0 || len(files) > 50 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择 1～50 个文件"})
			return
		}
		materials, failures := []exporter.Material{}, []string{}
		for _, file := range files {
			reader, err := file.Open()
			if err != nil {
				failures = append(failures, file.Filename+"：无法读取文件")
				continue
			}
			m, err := service.Import(r.FormValue("connectionId"), r.FormValue("kind"), file.Filename, reader)
			_ = reader.Close()
			if err != nil {
				failures = append(failures, file.Filename+"："+err.Error())
				continue
			}
			materials = append(materials, m)
		}
		writeJSON(w, http.StatusOK, map[string]any{"materials": materials, "errors": failures})
	})
	mux.HandleFunc("POST /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		var input exporter.JobInput
		if !decode(w, r, &input) {
			return
		}
		job, err := service.Start(input)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, ok := service.Job(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "任务不存在"})
			return
		}
		writeJSON(w, http.StatusOK, job)
	})
	mux.HandleFunc("POST /api/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		if err := service.Retry(r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := service.Cancel(r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/materials/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		_, path, err := service.File(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
		http.ServeFile(w, r, path)
	})
	mux.HandleFunc("GET /api/jobs/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		path, err := service.Archive(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "apaas-materials.zip"}))
		http.ServeFile(w, r, path)
	})
}
