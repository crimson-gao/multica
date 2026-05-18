package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func buildSkillZipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for path, content := range files {
		w, err := zw.Create(path)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", path, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry %s: %v", path, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func newSkillZipMultipart(t *testing.T, files map[string]string, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "skill.zip")
	if err != nil {
		t.Fatalf("create multipart file: %v", err)
	}
	if _, err := part.Write(buildSkillZipArchive(t, files)); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatalf("write multipart field %s: %v", key, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &body, mw.FormDataContentType()
}

func newSkillZipRequest(body *bytes.Buffer, contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/skills/import-zip", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	return req
}

func TestParseUploadedSkillZip_RejectsCompressedUploadOverLimit(t *testing.T) {
	_, err := parseUploadedSkillZip(
		bytes.NewReader(bytes.Repeat([]byte("x"), maxUploadSkillZipBytes+1)),
		"too-large.zip",
	)
	if err == nil {
		t.Fatal("expected oversized zip upload to fail")
	}
	if !strings.Contains(err.Error(), "1048576 byte limit") {
		t.Fatalf("error = %q, want 1 MiB byte limit", err.Error())
	}
}

func TestParseUploadedSkillZip_RejectsMultipleSkillFiles(t *testing.T) {
	data := buildSkillZipArchive(t, map[string]string{
		"SKILL.md":        "---\nname: root\n---\nroot",
		"nested/SKILL.md": "---\nname: nested\n---\nnested",
	})

	_, err := parseUploadedSkillZip(bytes.NewReader(data), "multi.zip")
	if err == nil {
		t.Fatal("expected zip with multiple SKILL.md files to fail")
	}
	if !strings.Contains(err.Error(), "exactly one skill") {
		t.Fatalf("error = %q, want exactly one skill", err.Error())
	}
}

func TestImportSkillZip_ConflictThenOverwriteReplacesExistingSkill(t *testing.T) {
	ctx := context.Background()
	safeName := strings.NewReplacer("/", "-", " ", "-").Replace(strings.ToLower(t.Name()))
	name := "zip-overwrite-" + safeName

	var existingID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO skill (workspace_id, name, description, content, config, created_by)
		VALUES ($1, $2, 'old description', 'old body', '{}'::jsonb, $3)
		RETURNING id
	`, testWorkspaceID, name, testUserID).Scan(&existingID); err != nil {
		t.Fatalf("insert existing skill: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM skill WHERE id = $1`, existingID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO skill_file (skill_id, path, content)
		VALUES ($1, 'old.md', 'old file')
	`, existingID); err != nil {
		t.Fatalf("insert existing skill file: %v", err)
	}

	files := map[string]string{
		"bundle/SKILL.md": "---\nname: " + name + "\ndescription: new description\n---\nnew body",
		"bundle/new.md":   "new file",
	}

	body, contentType := newSkillZipMultipart(t, files, nil)
	w := httptest.NewRecorder()
	testHandler.ImportSkillZip(w, newSkillZipRequest(body, contentType))
	if w.Code != http.StatusConflict {
		t.Fatalf("ImportSkillZip without overwrite: expected 409, got %d: %s", w.Code, w.Body.String())
	}

	body, contentType = newSkillZipMultipart(t, files, map[string]string{"overwrite": "true"})
	w = httptest.NewRecorder()
	testHandler.ImportSkillZip(w, newSkillZipRequest(body, contentType))
	if w.Code != http.StatusOK {
		t.Fatalf("ImportSkillZip with overwrite: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp SkillWithFilesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overwrite response: %v", err)
	}
	if resp.ID != existingID {
		t.Fatalf("overwrite response id = %s, want existing id %s", resp.ID, existingID)
	}
	if resp.Content != files["bundle/SKILL.md"] {
		t.Fatalf("content = %q, want imported SKILL.md", resp.Content)
	}

	var oldFileCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM skill_file WHERE skill_id = $1 AND path = 'old.md'`,
		existingID,
	).Scan(&oldFileCount); err != nil {
		t.Fatalf("count old file: %v", err)
	}
	if oldFileCount != 0 {
		t.Fatalf("old supporting file should be removed, got %d rows", oldFileCount)
	}

	var newFileContent string
	if err := testPool.QueryRow(ctx,
		`SELECT content FROM skill_file WHERE skill_id = $1 AND path = 'new.md'`,
		existingID,
	).Scan(&newFileContent); err != nil {
		t.Fatalf("load new file: %v", err)
	}
	if newFileContent != "new file" {
		t.Fatalf("new file content = %q, want new file", newFileContent)
	}
}
