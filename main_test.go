package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompileAndCache(t *testing.T) {
	// Initialize directories
	os.MkdirAll("data/pdfs", 0755)
	os.MkdirAll("data/icons", 0755)
	os.MkdirAll("data/builds", 0755)
	defer os.RemoveAll("data")

	// Create a dummy PDF
	dummyPDF := []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF")

	// Create a dummy PNG
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			img.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	var pngBuf bytes.Buffer
	err := png.Encode(&pngBuf, img)
	if err != nil {
		t.Fatalf("failed to encode dummy png: %v", err)
	}
	dummyPNG := pngBuf.Bytes()

	// Prepare multipart form data for Request 1
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	pdfWriter, err := writer.CreateFormFile("pdf", "test_doc.pdf")
	if err != nil {
		t.Fatal(err)
	}
	pdfWriter.Write(dummyPDF)

	iconWriter, err := writer.CreateFormFile("icon", "test_icon.png")
	if err != nil {
		t.Fatal(err)
	}
	iconWriter.Write(dummyPNG)

	_ = writer.WriteField("command", "Start-Process 'https://google.com'")
	_ = writer.WriteField("fileDescription", "Test Description")
	_ = writer.WriteField("productName", "Test Product")
	writer.Close()

	// Create httptest Server
	ts := httptest.NewServer(http.HandlerFunc(handleCompile))
	defer ts.Close()

	// 1. Run First Compile Request
	start := time.Now()
	resp1, err := http.Post(ts.URL, writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp1.Body)
		t.Fatalf("First compile failed with code %d: %s", resp1.StatusCode, string(bodyBytes))
	}

	var compileResp1 CompileResponse
	if err := json.NewDecoder(resp1.Body).Decode(&compileResp1); err != nil {
		t.Fatal(err)
	}

	elapsed1 := time.Since(start)
	t.Logf("First compile took %v, response: %+v", elapsed1, compileResp1)

	if !compileResp1.Success {
		t.Error("First compile success field was false")
	}

	if compileResp1.Filename != "test_doc.exe" {
		t.Errorf("Expected filename 'test_doc.exe', got '%s'", compileResp1.Filename)
	}

	// Verify build files exist
	buildHash := strings.TrimPrefix(compileResp1.DownloadURL, "/download/")
	exePath := filepath.Join("data/builds", buildHash+".exe")
	metaPath := filepath.Join("data/builds", buildHash+".json")

	if !fileExists(exePath) || !fileExists(metaPath) {
		t.Error("Compiled files do not exist in cache directory")
	}

	// 2. Run Second Compile Request (Verify Caching/De-duplication)
	var body2 bytes.Buffer
	writer2 := multipart.NewWriter(&body2)
	pdfWriter2, _ := writer2.CreateFormFile("pdf", "test_doc.pdf")
	pdfWriter2.Write(dummyPDF)
	iconWriter2, _ := writer2.CreateFormFile("icon", "test_icon.png")
	iconWriter2.Write(dummyPNG)
	_ = writer2.WriteField("command", "Start-Process 'https://google.com'")
	_ = writer2.WriteField("fileDescription", "Test Description")
	_ = writer2.WriteField("productName", "Test Product")
	writer2.Close()

	start = time.Now()
	resp2, err := http.Post(ts.URL, writer2.FormDataContentType(), &body2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var compileResp2 CompileResponse
	json.NewDecoder(resp2.Body).Decode(&compileResp2)

	elapsed2 := time.Since(start)
	t.Logf("Second compile took %v", elapsed2)

	if compileResp2.DownloadURL != compileResp1.DownloadURL {
		t.Errorf("Expected same build hash download URL, got '%s'", compileResp2.DownloadURL)
	}

	// Since it was cached, the elapsed time should be extremely short (under 50 milliseconds)
	if elapsed2 > 100*time.Millisecond {
		t.Errorf("Caching verification failed: second request took %v, expected < 100ms", elapsed2)
	}

	// 3. Test Download Route
	tsDownload := httptest.NewServer(http.HandlerFunc(handleDownload))
	defer tsDownload.Close()

	respDownload, err := http.Get(tsDownload.URL + compileResp1.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	defer respDownload.Body.Close()

	if respDownload.StatusCode != http.StatusOK {
		t.Fatalf("Download failed with code %d", respDownload.StatusCode)
	}

	contentDisposition := respDownload.Header.Get("Content-Disposition")
	if !strings.Contains(contentDisposition, `filename="test_doc.exe"`) {
		t.Errorf("Expected Content-Disposition to contain filename='test_doc.exe', got '%s'", contentDisposition)
	}

	// 4. Test Cleanup Expiration
	// Set the creation timestamp in metadata to 25 hours ago
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta BuildMetadata
	json.Unmarshal(metaBytes, &meta)
	meta.Created = time.Now().Add(-25 * time.Hour)

	newMetaBytes, _ := json.Marshal(meta)
	os.WriteFile(metaPath, newMetaBytes, 0644)

	// Run cleanupExpiredBuilds
	cleanupExpiredBuilds()

	// Verify they are cleaned up
	if fileExists(exePath) || fileExists(metaPath) {
		t.Error("Expired files were not cleaned up by purging worker")
	}

	// Check that pdfs and icons folders are cleaned up too (since they are no longer referenced)
	pdfFiles, _ := os.ReadDir("data/pdfs")
	iconFiles, _ := os.ReadDir("data/icons")
	if len(pdfFiles) > 0 || len(iconFiles) > 0 {
		t.Error("Orphaned cached files were not cleaned up from directories")
	}
}
