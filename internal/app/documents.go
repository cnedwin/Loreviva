package app

import (
	"context"
	"net/http"

	"loreviva.local/loreviva/internal/documents"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
)

func (r *Runtime) documentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /world/v1/admin/document-capabilities", r.adminControl("", "asset.read", func(ctx context.Context, scope domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		if len(q.URL.Query()) != 1 {
			return 0, nil, wire.ErrJSON
		}
		return 200, map[string]any{"parser_version": documents.ParserVersion, "formats": []string{"TXT", "MD", "DOCX", "PDF"}, "limits": map[string]int{"input_bytes": documents.MaxInputBytes, "expanded_bytes": documents.MaxExpandedBytes, "member_bytes": documents.MaxMemberBytes, "output_bytes": documents.MaxOutputBytes, "encoded_response_bytes": documents.MaxResponseBytes, "chunks": documents.MaxChunks, "pages": documents.MaxPages, "zip_entries": documents.MaxZIPEntries, "expansion_ratio": documents.MaxRatio, "xml_depth": documents.MaxDepth, "pdf_objects": documents.MaxPDFObjects, "tokens": documents.MaxTokens, "timeout_ms": int(documents.Timeout.Milliseconds())}, "read_only": true, "ocr": false, "execution_ready": false, "supported_subset": []string{"TXT/MD: literal UTF8 with original byte and line positions", "DOCX: ordinary WordprocessingML body; omitted containers are explicit", "PDF: Type1 Helvetica/Times/Courier with ASCII or WinAnsi, plain or Flate content; custom fonts/complex structures remain explicit gaps"}}, nil
	}))
	mux.HandleFunc("POST /world/v1/admin/assets/{asset_id}/document-preview", r.adminControl("DocumentPreviewRequest", "asset.read", func(ctx context.Context, scope domain.ProductScope, q *http.Request, raw []byte) (int, any, error) {
		request := decode[generated.DocumentPreviewRequest](raw)
		if len(q.URL.Query()) != 1 || q.URL.Query().Get("product_id") != scope.ProductID || string(request.ProductID) != scope.ProductID {
			return 0, nil, wire.ErrJSON
		}
		out, err := r.Files.PreviewDocument(ctx, scope, q.PathValue("asset_id"), request.ExpectedSha256)
		if err != nil {
			return 0, nil, err
		}
		// Repeat actual credential/expiry evaluation after parsing; the returned
		// preview remains scoped private bytes, never a transferable capability.
		if _, err = r.Auth.Authorize(ctx, q.Header.Get("Authorization"), scope.ProductID, "asset.read"); err != nil {
			return 0, nil, err
		}
		return 200, out, nil
	}))
}
