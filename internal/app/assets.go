package app

import (
	"context"
	"encoding/json"
	"io"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
)

func (r *Runtime) assetRoutes(m *http.ServeMux) {
	normalAsset := r.admin("", "asset.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		v, e := r.Files.Get(ctx, s, q.PathValue("asset_id"))
		return 200, v, e
	})
	m.HandleFunc("GET /world/v1/admin/assets/{asset_id}", func(w http.ResponseWriter, q *http.Request) {
		if candidateHistoryQuery(q) {
			if s, e := r.Auth.AuthorizeCommittedCandidateAsset(q.Context(), q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("asset_id")); e == nil {
				if e = r.committedArtifactReadReady(q.Context()); e != nil {
					failure(w, e)
					return
				}
				v, e := r.Files.Get(q.Context(), s, q.PathValue("asset_id"))
				if e != nil {
					failure(w, e)
					return
				}
				response(w, 200, v)
				return
			}
			if s, e := r.Auth.AuthorizeCommittedRehearsalReportAsset(q.Context(), q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("asset_id")); e == nil {
				if e = r.committedArtifactReadReady(q.Context()); e != nil {
					failure(w, e)
					return
				}
				v, e := r.Files.Get(q.Context(), s, q.PathValue("asset_id"))
				if e != nil {
					failure(w, e)
					return
				}
				response(w, 200, v)
				return
			}
		}
		normalAsset(w, q)
	})
	m.HandleFunc("GET /world/v1/assets/{asset_id}/content", func(w http.ResponseWriter, q *http.Request) {
		scope, e := r.assetReaderScope(q.Context(), q)
		if e != nil {
			if scope.Validate() == nil {
				failure(w, e)
			} else {
				failure(w, files.ErrNotFound)
			}
			return
		}
		meta, e := r.Files.Get(q.Context(), scope, q.PathValue("asset_id"))
		if e != nil {
			failure(w, files.ErrNotFound)
			return
		}
		reader, e := r.Files.OpenAuthorized(q.Context(), scope, q.PathValue("asset_id"))
		if e != nil {
			failure(w, files.ErrNotFound)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", meta.MediaType)
		w.Header().Set("Content-Length", string(meta.SizeBytes))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment")
		_, _ = io.Copy(w, reader)
	})
	m.HandleFunc("POST /world/v1/admin/assets", r.uploadAsset)
}
func (r *Runtime) uploadAsset(w http.ResponseWriter, q *http.Request) {
	kind, _, e := mime.ParseMediaType(q.Header.Get("Content-Type"))
	if e != nil || kind != "multipart/form-data" {
		failure(w, wire.ErrJSON)
		return
	}
	q.Body = http.MaxBytesReader(w, q.Body, files.MaxFileBytes+wire.MaxBytes+4096)
	mr, e := q.MultipartReader()
	if e != nil {
		failure(w, wire.ErrJSON)
		return
	}
	metadataPart, e := mr.NextPart()
	if e != nil || metadataPart.FormName() != "metadata" {
		failure(w, wire.ErrJSON)
		return
	}
	raw, e := io.ReadAll(io.LimitReader(metadataPart, wire.MaxBytes+1))
	metadataPart.Close()
	if e != nil || generated.Validate("AssetUploadMetadata", raw) != nil {
		failure(w, wire.ErrJSON)
		return
	}
	var metadata generated.AssetUploadMetadata
	if e = json.Unmarshal(raw, &metadata); e != nil {
		failure(w, wire.ErrJSON)
		return
	}
	scope, e := r.Auth.Authorize(q.Context(), q.Header.Get("Authorization"), string(metadata.ProductID), "asset.upload")
	if e != nil {
		failure(w, e)
		return
	}
	if e = r.Readiness(q.Context()); e != nil {
		failure(w, e)
		return
	}
	if domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
		failure(w, wire.ErrJSON)
		return
	}
	file, e := mr.NextPart()
	if e != nil || file.FormName() != "file" {
		failure(w, wire.ErrJSON)
		return
	}
	defer file.Close()
	record, e := r.Files.Upload(content.WithOriginalPayload(q.Context(), raw), scope, &finalMultipartReader{file: file, reader: mr}, metadata, q.Header.Get("Idempotency-Key"))
	if e != nil {
		failure(w, e)
		return
	}
	response(w, 201, record)
}

// Validate the final boundary before staging completes and before DB intake.
type finalMultipartReader struct {
	file     *multipart.Part
	reader   *multipart.Reader
	complete bool
}

func (r *finalMultipartReader) Read(p []byte) (int, error) {
	if r.complete {
		return 0, io.EOF
	}
	n, e := r.file.Read(p)
	if e == io.EOF {
		r.complete = true
		part, tail := r.reader.NextPart()
		if part != nil {
			part.Close()
		}
		if tail != io.EOF {
			return n, wire.ErrJSON
		}
	}
	return n, e
}
func (r *Runtime) capabilities(ctx context.Context, s domain.ProductScope, _ *http.Request, _ []byte) (int, any, error) {
	descriptor, _ := content.Hash(struct{ Connection, Model, Price string }{content.MockConnection, content.MockModel, "0"})
	out := map[string]any{"scope": content.Scope(s), "upload_max_bytes": strconv.Itoa(files.MaxFileBytes), "json_max_bytes": strconv.Itoa(wire.MaxBytes), "json_max_depth": wire.MaxDepth, "registered_primitives": []string{"SIGN_CUSTODY"}, "registered_schema_ids": []string{"DraftInput", "PublishCheckReport", "TurnRequest", "ApprovedPacket"}, "mandatory_duties": content.Duties, "routes": []any{map[string]any{"connection_ref": content.MockConnection, "model_id": content.MockModel, "descriptor_hash": descriptor, "capabilities": []string{"TEXT"}, "price_units": "0", "mode": "MOCK"}}, "supported_jobs": []string{"FILE_CLEANUP", "INSTANCE_CONTROL", "INSTANCE_MIGRATION"}, "unimplemented_jobs": []string{"REHEARSAL", "CANDIDATE", "PACKAGE_PREFLIGHT", "PACKAGE_IMPORT", "PACKAGE_EXPORT", "DOCUMENT_EXTRACTION"}, "mock_only": true}
	supported := []string{"FILE_CLEANUP", "INSTANCE_CONTROL", "INSTANCE_MIGRATION"}
	unimplemented := []string{}
	for _, job := range []string{"REHEARSAL", "CANDIDATE"} {
		ready := false
		if r.Worker != nil {
			if job == "REHEARSAL" {
				ready = r.Worker.Turns.RehearsalReady()
			} else {
				ready = r.Candidates != nil && r.Worker.Turns == r.Candidates.Executor && r.Worker.Turns.CandidateReady()
			}
		}
		if ready {
			supported = append(supported, job)
		} else {
			unimplemented = append(unimplemented, job)
		}
	}
	out["supported_jobs"] = supported
	out["unimplemented_jobs"] = append(unimplemented, "PACKAGE_PREFLIGHT", "PACKAGE_IMPORT", "PACKAGE_EXPORT", "DOCUMENT_EXTRACTION")
	return 200, out, nil
}
