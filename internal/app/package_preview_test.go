package app

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPackagePreviewMultipartIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields [][2]string
		want   bool
	}{
		{"complete", [][2]string{{"archive", "zip"}, {"options", `{"stage":"DISCOVER"}`}}, true},
		{"missing_options", [][2]string{{"archive", "zip"}}, false},
		{"duplicate_archive", [][2]string{{"archive", "zip"}, {"archive", "zip"}, {"options", "{}"}}, false},
		{"unknown_field", [][2]string{{"archive", "zip"}, {"options", "{}"}, {"unexpected", "ignored?"}}, false},
		{"oversized_options", [][2]string{{"archive", "zip"}, {"options", strings.Repeat(" ", 65537)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for _, field := range tc.fields {
				if err := writer.WriteField(field[0], field[1]); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/world/v1/admin/content-packages/preview?product_id=coffee", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			request.Header.Set("Idempotency-Key", "preview-multipart-key-0001")
			_, _, err := packagePreviewBody(request)
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want=%v", err == nil, tc.want)
			}
		})
	}
}
