package view_image

import (
	"strings"
	"testing"

	"her/tools"
)

// These tests only exercise Handle's guard clauses (bad args, missing
// image, missing vision client) — none of them reach vision.Describe /
// DescribeURL, so no network or LLM call happens.
func TestHandle_Guards(t *testing.T) {
	tests := []struct {
		name       string
		argsJSON   string
		ctx        *tools.Context
		wantSubstr string
	}{
		{
			name:       "no image, no image_url",
			argsJSON:   `{"prompt":"describe this"}`,
			ctx:        &tools.Context{},
			wantSubstr: "No image attached",
		},
		{
			name:       "image_url with bad scheme",
			argsJSON:   `{"prompt":"describe this","image_url":"ftp://example.com/cover.jpg"}`,
			ctx:        &tools.Context{},
			wantSubstr: "must be a direct http(s)",
		},
		{
			name:       "image_url with no scheme at all",
			argsJSON:   `{"prompt":"describe this","image_url":"example.com/cover.jpg"}`,
			ctx:        &tools.Context{},
			wantSubstr: "must be a direct http(s)",
		},
		{
			name:       "valid image_url but vision not configured",
			argsJSON:   `{"prompt":"describe this","image_url":"https://covers.openlibrary.org/b/id/8231856-M.jpg"}`,
			ctx:        &tools.Context{VisionLLM: nil},
			wantSubstr: "Vision is not configured",
		},
		{
			name:       "attached photo but vision not configured",
			argsJSON:   `{"prompt":"describe this"}`,
			ctx:        &tools.Context{ImageBase64: "Zm9v", ImageMIME: "image/jpeg"},
			wantSubstr: "Vision is not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Handle(tt.argsJSON, tt.ctx)
			if !strings.Contains(got, tt.wantSubstr) {
				t.Errorf("Handle() = %q, want substring %q", got, tt.wantSubstr)
			}
		})
	}
}
