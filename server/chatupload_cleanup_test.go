package server

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveUploadDoesNotOverwriteAndCleansStaging(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "complete attachment"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(&body, writer.Boundary())
	form, err := reader.ReadForm(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer form.RemoveAll()
	header := form.File["file"][0]
	dir := t.TempDir()
	dest := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(dest, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveUpload(header, dest); err == nil {
		t.Fatal("upload overwrote an existing attachment")
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "existing" {
		t.Fatalf("existing attachment changed: %q %v", got, err)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if err := saveUpload(header, dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "complete attachment" {
		t.Fatalf("completed attachment=%q %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("upload left staging files: %v %v", entries, err)
	}
}

type failingUploadReader struct{}

func (failingUploadReader) Read([]byte) (int, error) {
	return 0, errors.New("injected upload read failure")
}

func TestFailedUploadCopyLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "incomplete.txt")
	err := writeUpload(io.MultiReader(strings.NewReader("partial"), failingUploadReader{}), dest)
	if err == nil {
		t.Fatal("expected copy failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed upload left files: %v %v", entries, err)
	}
}
